package appconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPriority(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	systemDir := filepath.Join(root, "etc")
	for _, dir := range []string{userDir, systemDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i, tc := range []struct {
		dir  string
		want string
	}{{userDir, "user"}, {systemDir, "system"}} {
		path := filepath.Join(tc.dir, configFileName)
		if err := os.WriteFile(path, []byte(`{"dashboard":{"cardFilter":{"cards":["`+tc.want+`"]}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, selected, err := loadFromDirs(userDir, systemDir)
		if err != nil {
			t.Fatal(err)
		}
		if selected != path || cfg.Dashboard.CardFilter.Cards[0] != tc.want {
			t.Fatalf("priority %d: selected=%q cards=%v", i, selected, cfg.Dashboard.CardFilter.Cards)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadCreatesDefaultFileAndFillsDefaults(t *testing.T) {
	root := t.TempDir()
	home, etc := filepath.Join(root, "home"), filepath.Join(root, "etc")
	cfg, path, err := loadFromDirs(home, etc)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, configFileName) {
		t.Fatalf("path = %q", path)
	}
	if !cfg.Dashboard.Enabled || !cfg.Keys.Enabled || cfg.Dashboard.CardFilter.Mode != "blacklist" || cfg.Dashboard.CardFilter.Cards == nil {
		t.Fatalf("defaults not filled: %+v", cfg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default file not created: %v", err)
	}
	partial := filepath.Join(root, "partial.json")
	if err := os.WriteFile(partial, []byte(`{"keys":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = readConfig(partial)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.Enabled || cfg.Keys.Enabled || cfg.Dashboard.CardFilter.Mode != "blacklist" || cfg.Dashboard.CardFilter.Cards == nil {
		t.Fatalf("partial defaults not filled: %+v", cfg)
	}
}

func TestLoadInvalidJSONAndMode(t *testing.T) {
	for _, tc := range []struct{ contents, part string }{
		{"{broken", "JSON 解析失败"},
		{`{"dashboard":{"cardFilter":{"mode":"invalid"}}}`, "dashboard.cardFilter.mode"},
	} {
		path := filepath.Join(t.TempDir(), configFileName)
		if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readConfig(path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.part) {
			t.Fatalf("error = %v, want path and %q", err, tc.part)
		}
	}
}

func TestCardFilterVisibility(t *testing.T) {
	cases := []struct {
		name, mode string
		cards      []string
		provider   string
		account    string
		want       bool
	}{
		{"empty blacklist", "blacklist", nil, "p", "a", true},
		{"provider blacklist", "blacklist", []string{"p"}, "p", "a", false},
		{"account blacklist", "blacklist", []string{"p/a"}, "p", "a", false},
		{"other account", "blacklist", []string{"p/a"}, "p", "b", true},
		{"case sensitive", "blacklist", []string{"P"}, "p", "a", true},
		{"provider whitelist", "whitelist", []string{"p"}, "p", "a", true},
		{"account whitelist", "whitelist", []string{"p/a"}, "p", "a", true},
		{"whitelist miss", "whitelist", []string{"p/a"}, "p", "b", false},
		{"empty whitelist", "whitelist", []string{}, "p", "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (CardFilter{Mode: tc.mode, Cards: tc.cards}).VisibleCard(tc.provider, tc.account); got != tc.want {
				t.Errorf("VisibleCard() = %v, want %v", got, tc.want)
			}
		})
	}
}
