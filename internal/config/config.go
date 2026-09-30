// Package config 提供 ai-usage 运行时配置的加载与解析。
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 保存网关的运行时配置。
type Config struct {
	// Port 是 HTTP 监听端口，默认 8080。
	Port int
	// ListenHost 是 HTTP 监听地址，默认 127.0.0.1（仅本机），
	// 可配置为 0.0.0.0 或局域网 IP 以对外提供访问。
	ListenHost string
	// Passwd 是访问密码（Basic Auth，用户名固定 admin）。
	// 为空表示不启用认证（无认证模式，向后兼容）。
	// 注意：密码仅用于访问控制，不用于加密数据库（存储仍明文）。
	Passwd string
	// DataDir 是数据存储目录（SQLite 数据库、缓存等），默认 ~/.local/share/ai-usage。
	DataDir string
	// CacheTTL 是上游用量数据缓存的有效期，默认 5 分钟。
	CacheTTL time.Duration
}

// Load 从 flag 与环境变量加载配置，flag 显式设置的值优先于环境变量。
//
// 环境变量（未设置时使用内置默认值）：
//
//	AI_USAGE_PORT      监听端口（整数，默认 8080）
//	AI_USAGE_LISTEN    监听地址（默认 127.0.0.1）
//	AI_USAGE_PASSWD    访问密码（默认空，不启用认证）
//	AI_USAGE_DATA_DIR  数据目录（默认 ~/.local/share/ai-usage）
//	AI_USAGE_CACHE_TTL 缓存有效期（Go duration 字符串，如 5m，默认 5m）
//
// 命令行 flag（显式传入时覆盖环境变量，语义同上）：
//
//	-port       监听端口
//	-listen     监听地址
//	-passwd     访问密码
//	-data-dir   数据目录
//	-cache-ttl  缓存有效期
//
// DataDir 中开头的 ~ 会被展开为用户主目录的绝对路径。
// Load 可被多次调用。
func Load() (*Config, error) {
	cfg := &Config{
		Port:       8080,
		ListenHost: "127.0.0.1",
		Passwd:     "",
		CacheTTL:   5 * time.Minute,
	}

	var err error
	if cfg.DataDir, err = defaultDataDir(); err != nil {
		return nil, err
	}

	// 环境变量（存在时覆盖内置默认值）
	if cfg.Port, err = envInt("AI_USAGE_PORT", cfg.Port); err != nil {
		return nil, err
	}
	if cfg.ListenHost, err = envString("AI_USAGE_LISTEN", cfg.ListenHost); err != nil {
		return nil, err
	}
	if cfg.Passwd, err = envString("AI_USAGE_PASSWD", cfg.Passwd); err != nil {
		return nil, err
	}
	if cfg.DataDir, err = envString("AI_USAGE_DATA_DIR", cfg.DataDir); err != nil {
		return nil, err
	}
	if cfg.CacheTTL, err = envDuration("AI_USAGE_CACHE_TTL", cfg.CacheTTL); err != nil {
		return nil, err
	}

	// flag 显式传入时覆盖环境变量（包级注册避免重复注册 panic）
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = *portFlag
		case "listen":
			cfg.ListenHost = *listenFlag
		case "passwd":
			cfg.Passwd = *passwdFlag
		case "data-dir":
			cfg.DataDir = *dataDirFlag
		case "cache-ttl":
			cfg.CacheTTL = *cacheTTLFlag
		}
	})

	// 展开 ~ 为绝对路径
	cfg.DataDir, err = expandHome(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("展开 DataDir: %w", err)
	}

	return cfg, nil
}

var (
	portFlag     = flag.Int("port", 0, "HTTP 监听端口（env: AI_USAGE_PORT）")
	listenFlag   = flag.String("listen", "127.0.0.1", "HTTP 监听地址（env: AI_USAGE_LISTEN）")
	passwdFlag   = flag.String("passwd", "", "访问密码，Basic Auth 用户名 admin（env: AI_USAGE_PASSWD）")
	dataDirFlag  = flag.String("data-dir", "", "数据目录（env: AI_USAGE_DATA_DIR）")
	cacheTTLFlag = flag.Duration("cache-ttl", 0, "缓存有效期，如 5m（env: AI_USAGE_CACHE_TTL）")
)

func envInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，实际为 %q: %w", name, v, err)
	}
	return n, nil
}

func envString(name, def string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	return v, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是 duration（如 5m），实际为 %q: %w", name, v, err)
	}
	return d, nil
}

func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录: %w", err)
	}
	return filepath.Join(home, ".local", "share", "ai-usage"), nil
}

func expandHome(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录: %w", err)
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
