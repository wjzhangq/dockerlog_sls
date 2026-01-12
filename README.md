# Docker Log to SLS Agent

将 Docker 容器日志实时采集并发送到阿里云日志服务 (SLS) 的代理程序。

## 功能特性

- 监听 Docker 容器事件，自动采集新容器日志
- 支持扫描现有容器并恢复日志采集
- 使用 Docker 容器本地时间戳
- 启动时验证 SLS 访问权限
- 断点续传，记录采集位置

## 配置

### 配置文件示例 (`config.json`)

```json
{
  "docker": {
    "sock": "/var/run/docker.sock",
    "scan_existing": true
  },
  "sls": {
    "endpoint": "cn-hangzhou.log.aliyuncs.com",
    "access_key_id": "your-access-key-id",
    "access_key_secret": "your-access-key-secret",
    "project": "your-project",
    "logstore": "your-logstore"
  },
  "sync": {
    "offset_dir": "/var/run/dockerlog-sls",
    "flush_interval_seconds": 3,
    "max_rewind_lines": 100,
    "flush_size_limit": 31457280,
    "max_log_size": 65536
  },
  "env": "prod",
  "image_domain_map": {
    "test/test2": "www.test.com"
  }
}
```

### 配置项说明

| 配置项 | 说明 |
|--------|------|
| `docker.sock` | Docker socket 路径 |
| `docker.scan_existing` | 是否扫描现有容器 |
| `sls.endpoint` | SLS endpoint |
| `sls.access_key_id` | 阿里云 AccessKey ID |
| `sls.access_key_secret` | 阿里云 AccessKey Secret |
| `sls.project` | SLS 项目名称 |
| `sls.logstore` | SLS 日志库名称 |
| `sync.offset_dir` | 采集位置保存目录 |
| `sync.flush_interval_seconds` | 日志上传间隔（秒） |
| `sync.max_rewind_lines` | 重采最大回溯行数 |
| `sync.flush_size_limit` | 缓冲区大小限制（字节），超限自动上传，默认 30MB |
| `sync.max_log_size` | 单条日志最大长度，超长截断，默认 64KB |
| `env` | 环境标识（prod/test/dev） |
| `image_domain_map` | 镜像域名映射表 |

## 使用方法

```bash
# 构建
go build -o dockerlog_sls

# 运行
./dockerlog_sls config.json
```

## 日志字段

上传到 SLS 的日志包含以下字段：

| 字段 | 说明 |
|------|------|
| `message` | 日志内容 |
| `container_id` | 容器短 ID |
| `container` | 容器名称 |
| `image` | 镜像名称 |
| `tag` | 镜像标签 |
| `ports` | 端口映射 |
| `host_ip` | 主机 IP |
| `log_stream` | 日志流类型（stdout/stderr） |
| `domain` | 镜像域名 |
| `env` | 环境标识 |

## 输出示例

```
2025/01/07 10:00:00 [SLS] Verifying access to project=myproject, logstore=mylogstore
2025/01/07 10:00:00 [SLS] Access verified successfully
2025/01/07 10:00:00 [Docker] Found 2 existing containers:
2025/01/07 10:00:00   - nginx (abc12345)
2025/01/07 10:00:00   - redis (def67890)
2025/01/07 10:00:00 [nginx] Start streaming logs
2025/01/07 10:00:01 [Docker] Container started: myapp (xyz11111)
2025/01/07 10:00:03 [Docker] Container stop: myapp (xyz11111)
```
