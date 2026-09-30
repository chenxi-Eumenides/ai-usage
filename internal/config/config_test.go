package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("AI_USAGE_PORT")
	os.Unsetenv("AI_USAGE_LISTEN")
	os.Unsetenv("AI_USAGE_PASSWD")
	os.Unsetenv("AI_USAGE_DATA_DIR")
	os.Unsetenv("AI_USAGE_CACHE_TTL")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.ListenHost != "127.0.0.1" {
		t.Errorf("ListenHost = %q, want 127.0.0.1", cfg.ListenHost)
	}
	if cfg.Passwd != "" {
		t.Errorf("Passwd = %q, want empty", cfg.Passwd)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "share", "ai-usage")
	if cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if cfg.CacheTTL != 5*time.Minute {
		t.Errorf("CacheTTL = %v, want 5m", cfg.CacheTTL)
	}
}

func TestLoadEnvAndTilde(t *testing.T) {
	os.Setenv("AI_USAGE_PORT", "9090")
	os.Setenv("AI_USAGE_DATA_DIR", "~/t1-data")
	os.Setenv("AI_USAGE_CACHE_TTL", "1h")
	defer func() {
		os.Unsetenv("AI_USAGE_PORT")
		os.Unsetenv("AI_USAGE_DATA_DIR")
		os.Unsetenv("AI_USAGE_CACHE_TTL")
	}()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "t1-data")
	if cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if cfg.CacheTTL != time.Hour {
		t.Errorf("CacheTTL = %v, want 1h", cfg.CacheTTL)
	}
}

func TestLoadInvalidEnv(t *testing.T) {
	os.Setenv("AI_USAGE_PORT", "not-a-number")
	defer os.Unsetenv("AI_USAGE_PORT")
	if _, err := Load(); err == nil {
		t.Fatal("want error for invalid PORT, got nil")
	}
}

func TestLoadListenPasswdEnv(t *testing.T) {
	os.Setenv("AI_USAGE_LISTEN", "0.0.0.0")
	os.Setenv("AI_USAGE_PASSWD", "secret123")
	defer func() {
		os.Unsetenv("AI_USAGE_LISTEN")
		os.Unsetenv("AI_USAGE_PASSWD")
	}()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenHost != "0.0.0.0" {
		t.Errorf("ListenHost = %q, want 0.0.0.0", cfg.ListenHost)
	}
	if cfg.Passwd != "secret123" {
		t.Errorf("Passwd = %q, want secret123", cfg.Passwd)
	}
}

// flag 显式设置优先于环境变量。用 t.Cleanup 把 flag 值恢复为默认，
// 即使测试乱序执行也不污染其他用例（flag 默认值与 cfg 默认一致）。
func TestLoadListenPasswdFlag(t *testing.T) {
	os.Unsetenv("AI_USAGE_LISTEN")
	os.Unsetenv("AI_USAGE_PASSWD")
	if err := flag.Set("listen", "0.0.0.0"); err != nil {
		t.Fatalf("flag.Set(listen): %v", err)
	}
	if err := flag.Set("passwd", "flagpass"); err != nil {
		t.Fatalf("flag.Set(passwd): %v", err)
	}
	t.Cleanup(func() {
		if err := flag.Set("listen", "127.0.0.1"); err != nil {
			t.Errorf("restore listen: %v", err)
		}
		if err := flag.Set("passwd", ""); err != nil {
			t.Errorf("restore passwd: %v", err)
		}
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenHost != "0.0.0.0" {
		t.Errorf("ListenHost = %q, want 0.0.0.0 (flag overrides env)", cfg.ListenHost)
	}
	if cfg.Passwd != "flagpass" {
		t.Errorf("Passwd = %q, want flagpass (flag overrides env)", cfg.Passwd)
	}
}
