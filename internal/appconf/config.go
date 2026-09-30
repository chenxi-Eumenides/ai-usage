// Package appconf 加载用户级 JSON 应用配置。
package appconf

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// ProjectName 是用户配置目录名；项目名待定，改此常量即可。
const ProjectName = "ai-usage"

const configFileName = "config.json"

type Config struct {
	Dashboard DashboardConfig `json:"dashboard"`
	Keys      KeysConfig      `json:"keys"`
}

type DashboardConfig struct {
	Enabled    bool       `json:"enabled"`
	CardFilter CardFilter `json:"cardFilter"`
}

type KeysConfig struct {
	Enabled bool `json:"enabled"`
}

type CardFilter struct {
	Mode  string   `json:"mode"`
	Cards []string `json:"cards"`
}

// Features 是注入页面的功能开关及卡片过滤配置。
type Features struct {
	DashboardEnabled bool       `json:"dashboardEnabled"`
	KeysEnabled      bool       `json:"keysEnabled"`
	CardFilter       CardFilter `json:"cardFilter"`
}

func Default() Config {
	return Config{
		Dashboard: DashboardConfig{Enabled: true, CardFilter: CardFilter{Mode: "blacklist", Cards: []string{}}},
		Keys:      KeysConfig{Enabled: true},
	}
}

func (c Config) PageFeatures() Features {
	return Features{
		DashboardEnabled: c.Dashboard.Enabled,
		KeysEnabled:      c.Keys.Enabled,
		CardFilter:       c.Dashboard.CardFilter,
	}
}

// Load 按可执行文件同目录、用户配置目录、系统配置目录的优先级加载配置。
// 返回生效配置、选中的文件路径和错误；首次缺少配置时尝试创建默认文件。
func Load() (Config, string, error) {
	executable, err := os.Executable()
	if err != nil {
		executable, err = os.Getwd()
		if err != nil {
			return Config{}, "", fmt.Errorf("获取当前工作目录失败: %w", err)
		}
	}
	executableDir := filepath.Dir(executable)
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return loadFromDirs(executableDir, filepath.Join(home, ".config", ProjectName), filepath.Join("/etc", ProjectName))
}

func loadFromDirs(executableDir, userDir, systemDir string) (Config, string, error) {
	paths := []string{
		filepath.Join(executableDir, configFileName),
		filepath.Join(userDir, configFileName),
		filepath.Join(systemDir, configFileName),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			cfg, parseErr := readConfig(path)
			return cfg, path, parseErr
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, path, fmt.Errorf("检查配置文件 %s 失败: %w", path, err)
		}
	}

	cfg := Default()
	path := paths[0]
	contents, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		contents = append(contents, '\n')
		err = os.WriteFile(path, contents, 0600)
	}
	if err != nil {
		log.Printf("警告：无法创建默认配置文件 %s：%v；继续使用默认配置", path, err)
	}
	return cfg, path, nil
}

func readConfig(path string) (Config, error) {
	cfg := Default()
	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, fmt.Errorf("配置文件 %s JSON 解析失败: %w", path, err)
	}
	if cfg.Dashboard.CardFilter.Mode != "blacklist" && cfg.Dashboard.CardFilter.Mode != "whitelist" {
		return Config{}, fmt.Errorf("配置文件 %s 字段 dashboard.cardFilter.mode 值无效 %q（只允许 blacklist 或 whitelist）", path, cfg.Dashboard.CardFilter.Mode)
	}
	return cfg, nil
}

// VisibleCard 判断 provider/account 卡片是否通过 cardFilter。
func (f CardFilter) VisibleCard(provider, account string) bool {
	if len(f.Cards) == 0 {
		return true
	}
	matched := false
	for _, card := range f.Cards {
		if card == provider || card == provider+"/"+account {
			matched = true
			break
		}
	}
	if f.Mode == "whitelist" {
		return matched
	}
	return !matched
}
