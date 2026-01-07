package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sls "github.com/aliyun/aliyun-log-go-sdk"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"google.golang.org/protobuf/proto"
)

/* =======================
   Config
======================= */

type Config struct {
	Docker struct {
		Sock         string `json:"sock"`
		ScanExisting bool   `json:"scan_existing"`
	} `json:"docker"`

	SLS struct {
		Endpoint        string `json:"endpoint"`
		AccessKeyID     string `json:"access_key_id"`
		AccessKeySecret string `json:"access_key_secret"`
		Project         string `json:"project"`
		Logstore        string `json:"logstore"`
	} `json:"sls"`

	Sync struct {
		OffsetDir            string `json:"offset_dir"`
		FlushIntervalSeconds int    `json:"flush_interval_seconds"`
		MaxRewindLines       int    `json:"max_rewind_lines"`
	} `json:"sync"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, json.Unmarshal(b, &cfg)
}

/* =======================
   Container Meta
======================= */

type ContainerMeta struct {
	ID       string
	Name     string
	Image    string
	ImageTag string
	Ports    string
	HostIP   string
}

/* =======================
   Container Manager
======================= */

type ContainerManager struct {
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func NewContainerManager() *ContainerManager {
	return &ContainerManager{
		running: make(map[string]context.CancelFunc),
	}
}

func (m *ContainerManager) Start(
	parent context.Context,
	cli *client.Client,
	slsClient *sls.Client,
	cfg *Config,
	containerID string,
) {
	m.mu.Lock()
	if _, ok := m.running[containerID]; ok {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	m.running[containerID] = cancel
	m.mu.Unlock()

	go func() {
		defer m.Stop(containerID)
		handleContainer(ctx, cli, slsClient, cfg, containerID)
	}()
}

func (m *ContainerManager) Stop(containerID string) {
	m.mu.Lock()
	if cancel, ok := m.running[containerID]; ok {
		cancel()
		delete(m.running, containerID)
	}
	m.mu.Unlock()
}

/* =======================
   Docker Handling
======================= */

func scanExistingContainers(
	ctx context.Context,
	cli *client.Client,
	mgr *ContainerManager,
	slsClient *sls.Client,
	cfg *Config,
) {
	list, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		log.Println("list container error:", err)
		return
	}
	for _, c := range list {
		mgr.Start(ctx, cli, slsClient, cfg, c.ID)
	}
}

func watchDockerEvents(
	ctx context.Context,
	cli *client.Client,
	mgr *ContainerManager,
	slsClient *sls.Client,
	cfg *Config,
) {
	evCh, errCh := cli.Events(ctx, types.EventsOptions{})

	for {
		select {
		case e := <-evCh:
			if e.Type != events.ContainerEventType {
				continue
			}
			switch e.Action {
			case "start":
				mgr.Start(ctx, cli, slsClient, cfg, e.ID)
			case "die", "stop", "destroy":
				mgr.Stop(e.ID)
			}
		case err := <-errCh:
			log.Println("docker event error:", err)
			time.Sleep(time.Second)
		case <-ctx.Done():
			return
		}
	}
}

func handleContainer(
	ctx context.Context,
	cli *client.Client,
	slsClient *sls.Client,
	cfg *Config,
	containerID string,
) {
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return
	}

	image := inspect.Config.Image
	name, tag := image, "latest"
	if strings.Contains(image, ":") {
		p := strings.SplitN(image, ":", 2)
		name, tag = p[0], p[1]
	}

	meta := ContainerMeta{
		ID:       inspect.ID[:12],
		Name:     strings.TrimPrefix(inspect.Name, "/"),
		Image:    name,
		ImageTag: tag,
		Ports:    parsePorts(inspect),
		HostIP:   getHostIP(),
	}

	streamLogs(ctx, cli, slsClient, cfg, meta)
}

/* =======================
   Ports / Host IP
======================= */

func parsePorts(inspect types.ContainerJSON) string {
	var ports []string
	for p, bindings := range inspect.NetworkSettings.Ports {
		for _, b := range bindings {
			s := fmt.Sprintf("%s->%s", p.Port(), b.HostPort)
			if p.Proto() == "udp" {
				s += "/udp"
			}
			ports = append(ports, s)
		}
	}
	sort.Strings(ports)
	return strings.Join(ports, ",")
}

func getHostIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "unknown"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

/* =======================
   Offset
======================= */

type Offset struct {
	Line int `json:"line"`
}

func loadOffset(path string, rewind int) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var o Offset
	if json.Unmarshal(b, &o) != nil {
		return 0
	}
	if o.Line > rewind {
		return o.Line - rewind
	}
	return 0
}

func saveOffset(path string, line int) {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	b, _ := json.Marshal(Offset{Line: line})
	_ = os.WriteFile(path, b, 0644)
}

/* =======================
   Log Streaming
======================= */

func streamLogs(
	ctx context.Context,
	cli *client.Client,
	slsClient *sls.Client,
	cfg *Config,
	meta ContainerMeta,
) {
	offsetFile := filepath.Join(cfg.Sync.OffsetDir, meta.ID+".json")
	startLine := loadOffset(offsetFile, cfg.Sync.MaxRewindLines)

	reader, err := cli.ContainerLogs(ctx, meta.ID, types.ContainerLogsOptions{
		Follow:     true,
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
	})
	if err != nil {
		return
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	lineNum := 0
	var buf []*sls.Log
	lastFlush := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if !scanner.Scan() {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			lineNum++
			if lineNum <= startLine {
				continue
			}

			buf = append(buf, buildLog(meta, scanner.Text()))

			if time.Since(lastFlush) >= time.Second*time.Duration(cfg.Sync.FlushIntervalSeconds) {
				if len(buf) > 0 {
					putLogs(slsClient, cfg, meta, buf)
					saveOffset(offsetFile, lineNum)
					buf = nil
				}
				lastFlush = time.Now()
			}
		}
	}
}

/* =======================
   SLS
======================= */

func buildLog(meta ContainerMeta, msg string) *sls.Log {
	t := uint32(time.Now().Unix())
	return &sls.Log{
		Time: &t,
		Contents: []*sls.LogContent{
			{Key: proto.String("message"), Value: proto.String(msg)},
			{Key: proto.String("container"), Value: proto.String(meta.Name)},
			{Key: proto.String("image"), Value: proto.String(meta.Image)},
			{Key: proto.String("tag"), Value: proto.String(meta.ImageTag)},
			{Key: proto.String("ports"), Value: proto.String(meta.Ports)},
			{Key: proto.String("host_ip"), Value: proto.String(meta.HostIP)},
		},
	}
}

func putLogs(
	client *sls.Client,
	cfg *Config,
	meta ContainerMeta,
	logs []*sls.Log,
) {
	topic := meta.Name
	source := meta.HostIP

	lg := &sls.LogGroup{
		Topic:  &topic,
		Source: &source,
		Logs:   logs,
	}

	err := client.PutLogs(cfg.SLS.Project, cfg.SLS.Logstore, lg)
	if err != nil {
		log.Println("sls put error:", err)
	}
}

/* =======================
   Main
======================= */

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: ./agent config.json")
	}

	cfg, err := loadConfig(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+cfg.Docker.Sock),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatal(err)
	}

	slsClient := &sls.Client{
		Endpoint:        cfg.SLS.Endpoint,
		AccessKeyID:     cfg.SLS.AccessKeyID,
		AccessKeySecret: cfg.SLS.AccessKeySecret,
		SecurityToken:   "",
	}

	ctx := context.Background()
	mgr := NewContainerManager()

	if cfg.Docker.ScanExisting {
		scanExistingContainers(ctx, cli, mgr, slsClient, cfg)
	}

	watchDockerEvents(ctx, cli, mgr, slsClient, cfg)
}
