package main

import (
	"bufio"
	"context"
	"encoding/json"
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
		FlushSizeLimit       int    `json:"flush_size_limit"`
		MaxLogSize           int    `json:"max_log_size"`
	} `json:"sync"`

	Env               string            `json:"env"`
	ImageDomainMap    map[string]string `json:"image_domain_map"`
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
	ID          string
	ContainerID string
	Name        string
	Image       string
	ImageTag    string
	Ports       string
	HostIP      string
	LogStream   string
	Domain      string
	Env         string
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

	// Print all existing containers
	log.Printf("[Docker] Found %d existing containers:", len(list))
	for _, c := range list {
		inspect, _ := cli.ContainerInspect(ctx, c.ID)
		name := strings.TrimPrefix(inspect.Name, "/")
		log.Printf("  - %s (%s)", name, c.ID[:12])
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
			shortID := e.ID[:12]
			switch e.Action {
			case "start":
				// Get container name for logging
				inspect, _ := cli.ContainerInspect(ctx, e.ID)
				name := strings.TrimPrefix(inspect.Name, "/")
				log.Printf("[Docker] Container started: %s (%s)", name, shortID)
				mgr.Start(ctx, cli, slsClient, cfg, e.ID)
			case "die", "stop", "destroy":
				// Get container name for logging
				inspect, _ := cli.ContainerInspect(ctx, e.ID)
				name := strings.TrimPrefix(inspect.Name, "/")
				log.Printf("[Docker] Container %s: %s (%s)", e.Action, name, shortID)
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
		// Handle registry URL: registry-sy.xcloud.lenovo.com/smart-meeting-summary/service:tag
		// Extract image path after registry, keep full path
		if strings.Contains(p[0], "/") {
			// Find first slash after registry, keep everything after it
			firstSlash := strings.Index(p[0], "/")
			name = p[0][firstSlash+1:]
		} else {
			name = p[0]
		}
		tag = p[1]
	}
	// Remove leading / if present
	name = strings.TrimPrefix(name, "/")

	meta := ContainerMeta{
		ID:          inspect.ID[:12],
		ContainerID: inspect.ID[:12],
		Name:        strings.TrimPrefix(inspect.Name, "/"),
		Image:       name,
		ImageTag:    tag,
		Ports:       parsePorts(inspect),
		HostIP:      getHostIP(cfg.SLS.Endpoint),
		LogStream:   "stdout/stderr",
		Domain:      getDomain(name, cfg.ImageDomainMap),
		Env:         cfg.Env,
	}

	streamLogs(ctx, cli, slsClient, cfg, meta)
}

/* =======================
   Ports / Host IP
======================= */

func parsePorts(inspect types.ContainerJSON) string {
	portSet := make(map[string]struct{})
	for p := range inspect.NetworkSettings.Ports {
		port := p.Port()
		if p.Proto() == "udp" {
			port += "/udp"
		}
		portSet[port] = struct{}{}
	}

	var ports []string
	for port := range portSet {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	return strings.Join(ports, ",")
}

func getHostIP(endpoint string) string {
	host, _, _ := net.SplitHostPort(endpoint)
	if host == "" {
		host = endpoint
	}
	conn, err := net.Dial("udp", host+":80")
	if err != nil {
		return "unknown"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func getDomain(image string, domainMap map[string]string) string {
	if domain, ok := domainMap[image]; ok {
		return domain
	}
	return ""
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

	log.Printf("[%s] Start streaming logs", meta.Name)

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

			logLine := scanner.Text()
			timestamp, msg := parseDockerTimestamp(logLine, cfg.Sync.MaxLogSize)
			buf = append(buf, buildLog(meta, msg, timestamp))

			// Flush if size limit reached
			if shouldFlushBySize(buf, cfg.Sync.FlushSizeLimit) {
				putLogs(slsClient, cfg, meta, buf)
				saveOffset(offsetFile, lineNum)
				buf = nil
				lastFlush = time.Now()
			} else if time.Since(lastFlush) >= time.Second*time.Duration(cfg.Sync.FlushIntervalSeconds) {
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

// shouldFlushBySize checks if buffer size exceeds limit (in bytes)
// SLS limit is 33550336 bytes (32MB)
func shouldFlushBySize(buf []*sls.Log, limit int) bool {
	if limit <= 0 {
		return false
	}
	var size int
	for _, log := range buf {
		for _, c := range log.Contents {
			size += len(c.GetKey()) + len(c.GetValue())
		}
	}
	return size >= limit
}

// parseDockerTimestamp parses Docker log line with timestamp
// Docker format: "2006-01-02T15:04:05.999999999Z07:00 log message"
func parseDockerTimestamp(line string, maxSize int) (uint32, string) {
	// Try to parse RFC3339 timestamp
	fields := strings.SplitN(line, " ", 2)
	if len(fields) >= 2 {
		if ts, err := time.Parse(time.RFC3339Nano, fields[0]); err == nil {
			msg := fields[1]
			if maxSize > 0 && len(msg) > maxSize {
				msg = msg[:maxSize]
			}
			return uint32(ts.Unix()), msg
		}
	}
	// Fallback: use current time
	msg := line
	if maxSize > 0 && len(msg) > maxSize {
		msg = msg[:maxSize]
	}
	return uint32(time.Now().Unix()), msg
}

/* =======================
   SLS
======================= */

func buildLog(meta ContainerMeta, msg string, timestamp uint32) *sls.Log {
	return &sls.Log{
		Time: &timestamp,
		Contents: []*sls.LogContent{
			{Key: proto.String("message"), Value: proto.String(msg)},
			{Key: proto.String("container_id"), Value: proto.String(meta.ContainerID)},
			{Key: proto.String("container"), Value: proto.String(meta.Name)},
			{Key: proto.String("image"), Value: proto.String(meta.Image)},
			{Key: proto.String("tag"), Value: proto.String(meta.ImageTag)},
			{Key: proto.String("ports"), Value: proto.String(meta.Ports)},
			{Key: proto.String("host_ip"), Value: proto.String(meta.HostIP)},
			{Key: proto.String("log_stream"), Value: proto.String(meta.LogStream)},
			{Key: proto.String("domain"), Value: proto.String(meta.Domain)},
			{Key: proto.String("env"), Value: proto.String(meta.Env)},
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

	// Verify SLS access
	log.Printf("[SLS] Verifying access to project=%s, logstore=%s", cfg.SLS.Project, cfg.SLS.Logstore)
	_, err = slsClient.GetLogStore(cfg.SLS.Project, cfg.SLS.Logstore)
	if err != nil {
		log.Fatalf("[SLS] Failed to verify access: %v", err)
	}
	log.Printf("[SLS] Access verified successfully")

	ctx := context.Background()
	mgr := NewContainerManager()

	if cfg.Docker.ScanExisting {
		scanExistingContainers(ctx, cli, mgr, slsClient, cfg)
	}

	watchDockerEvents(ctx, cli, mgr, slsClient, cfg)
}
