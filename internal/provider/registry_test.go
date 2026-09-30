package provider

import (
	"context"
	"errors"
	"testing"
)

// stubFetch 是测试用的 FetchFunc：返回固定余额 Usage。
func stubFetch(ctx context.Context, key string, keyType string) (*Usage, error) {
	return &Usage{BalanceType: "balance"}, nil
}

// resetRegistry 清空注册表，确保测试隔离。
func resetRegistry() {
	mu.Lock()
	defer mu.Unlock()
	providers = make(map[string]Provider)
	aliases = make(map[string]string)
}

func TestRegisterAndGet(t *testing.T) {
	resetRegistry()

	spec := Spec{ID: "test-provider", DisplayName: "Test Provider", Aliases: []string{"tp"}}
	if err := Register(spec, stubFetch); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	got, ok := Get("test-provider")
	if !ok {
		t.Fatal("Get returned not found")
	}
	if got.ID() != "test-provider" {
		t.Errorf("expected id test-provider, got %s", got.ID())
	}
	if got.DisplayName() != "Test Provider" {
		t.Errorf("expected display from spec, got %q", got.DisplayName())
	}
}

func TestGetByAlias(t *testing.T) {
	resetRegistry()

	spec := Spec{ID: "deepseek", Aliases: []string{"deepseek", "ds"}}
	Register(spec, stubFetch)

	// 通过主别名命中
	got, ok := GetByAlias("deepseek")
	if !ok {
		t.Fatal("GetByAlias('deepseek') returned not found")
	}
	if got.ID() != "deepseek" {
		t.Errorf("expected id deepseek, got %s", got.ID())
	}

	// 通过次别名命中
	got, ok = GetByAlias("ds")
	if !ok || got.ID() != "deepseek" {
		t.Errorf("GetByAlias('ds') should hit deepseek, got %v %v", ok, got)
	}

	// 不存在的别名返回 false
	_, ok = GetByAlias("nonexistent")
	if ok {
		t.Fatal("GetByAlias('nonexistent') should return false")
	}
}

func TestDuplicateRegister(t *testing.T) {
	resetRegistry()

	spec := Spec{ID: "dup"}
	if err := Register(spec, stubFetch); err != nil {
		t.Fatalf("first Register failed: %v", err)
	}
	if err := Register(spec, stubFetch); err == nil {
		t.Fatal("expected error on duplicate register, got nil")
	}
}

func TestRegisterValidation(t *testing.T) {
	resetRegistry()

	if err := Register(Spec{}, stubFetch); err == nil {
		t.Error("expected error for empty ID, got nil")
	}
	if err := Register(Spec{ID: "no-fetch"}, nil); err == nil {
		t.Error("expected error for nil fetch, got nil")
	}
	resetRegistry()
	if err := Register(Spec{ID: "invalid-credential", Credential: &CredentialSpec{Kind: "password"}}, stubFetch); err == nil {
		t.Error("expected error for invalid credential kind, got nil")
	}
}

func TestRegisterRejectsInvalidProxyURL(t *testing.T) {
	for _, proxyURL := range []string{
		"localhost:20170",
		"ftp://localhost:20170",
		"http://:20170",
	} {
		t.Run(proxyURL, func(t *testing.T) {
			resetRegistry()
			err := Register(Spec{ID: "invalid-proxy", ProxyURL: proxyURL}, stubFetch)
			if err == nil {
				t.Fatalf("Register(%q) error = nil, want invalid proxy error", proxyURL)
			}
		})
	}
}

func TestRegisterAcceptsSOCKS5ProxyURL(t *testing.T) {
	resetRegistry()
	if err := Register(Spec{ID: "socks5-provider", ProxyURL: "socks5://localhost:20170"}, stubFetch); err != nil {
		t.Fatalf("Register SOCKS5 proxy failed: %v", err)
	}
}

func TestSpecAdapterFetch(t *testing.T) {
	resetRegistry()

	// FetchUsage 透传给注册的 fetch 函数
	if err := Register(Spec{ID: "fetchable"}, stubFetch); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	got, ok := Get("fetchable")
	if !ok {
		t.Fatal("Get returned not found")
	}
	u, err := got.FetchUsage(context.Background(), "k", UsageTypeBalance)
	if err != nil || u == nil || u.BalanceType != "balance" {
		t.Errorf("FetchUsage should delegate to fetch func, got %+v %v", u, err)
	}
}

func TestSpecAdapterInjectsProxyURL(t *testing.T) {
	resetRegistry()

	const wantProxyURL = "http://localhost:20170"
	var receivedProxyURL string
	fetch := func(ctx context.Context, key string, keyType string) (*Usage, error) {
		proxyURL := proxyURLFromContext(ctx)
		if proxyURL == nil {
			t.Fatal("fetch context is missing ProxyURL")
		}
		receivedProxyURL = proxyURL.String()
		return &Usage{BalanceType: "balance"}, nil
	}
	if err := Register(Spec{ID: "proxied", ProxyURL: wantProxyURL}, fetch); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	p, ok := Get("proxied")
	if !ok {
		t.Fatal("Get returned not found")
	}
	if _, err := p.FetchUsage(context.Background(), "key", UsageTypeBalance); err != nil {
		t.Fatalf("FetchUsage failed: %v", err)
	}
	if receivedProxyURL != wantProxyURL {
		t.Errorf("injected proxy URL = %q, want %q", receivedProxyURL, wantProxyURL)
	}
}

func TestFallback(t *testing.T) {
	p := Fallback("unknown")

	if p.ID() != "unknown" {
		t.Errorf("expected id 'unknown', got %q", p.ID())
	}
	if p.DisplayName() != "unknown" {
		t.Errorf("expected display name 'unknown', got %q", p.DisplayName())
	}
	if len(p.Aliases()) != 0 {
		t.Error("fallback should have no aliases")
	}
	if len(p.KeyPrefixes()) != 0 {
		t.Error("fallback should have no key prefixes")
	}
	if p.UsageNote() != "" {
		t.Error("fallback should have empty usage note")
	}

	// FetchUsage 返回 ErrNotSupported，可用 errors.Is 识别
	_, err := p.FetchUsage(context.Background(), "test-key", UsageTypeBalance)
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("fallback FetchUsage should return ErrNotSupported, got %v", err)
	}
}

func TestFallbackNotRegistered(t *testing.T) {
	resetRegistry()

	// fallback 不经过 Register()，GetByAlias 查不到
	got, ok := GetByAlias("unknown-fb")
	if ok {
		t.Errorf("GetByAlias should not find unregistered fallback, got %v", got)
	}
	_ = got
}

func TestAllSorted(t *testing.T) {
	resetRegistry()

	Register(Spec{ID: "c"}, stubFetch)
	Register(Spec{ID: "a"}, stubFetch)
	Register(Spec{ID: "b"}, stubFetch)

	all := All()
	if len(all) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(all))
	}

	// 验证按 id 排序
	for i := 1; i < len(all); i++ {
		if all[i-1].ID() >= all[i].ID() {
			t.Errorf("providers not sorted: %s >= %s", all[i-1].ID(), all[i].ID())
		}
	}

	// 跨调用稳定顺序
	all2 := All()
	for i := range all {
		if all[i].ID() != all2[i].ID() {
			t.Errorf("All() not stable: all[%d]=%s, all2[%d]=%s", i, all[i].ID(), i, all2[i].ID())
		}
	}
}
