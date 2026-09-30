package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-usage/internal/appconf"
	"ai-usage/internal/cache"
	"ai-usage/internal/provider"
	"ai-usage/internal/store"
)

// ---------------------------------------------------------------------------
// mock provider
// ---------------------------------------------------------------------------

// mockProvider 是可编程的测试 provider。calls 统计 FetchUsage 调用次数。
// balanceUsage/planUsage 分别配置 balance/plan 两种类型的返回，未配置的类型
// 返回 ErrNotSupported（模拟「该平台不支持此类型」）。
type mockProvider struct {
	id           string
	display      string
	aliases      []string
	prefixes     []string
	keyPattern   string
	console      string
	consoleUsage string
	note         string
	meta         provider.PlanMeta
	usageType    string
	balanceUsage *provider.Usage
	balanceErr   error
	planUsage    *provider.Usage
	planErr      error
	calls        *atomic.Int64
}

type cookieMockProvider struct {
	*mockProvider
	credential string
}

func (m *cookieMockProvider) CredentialSpec() *provider.CredentialSpec {
	return &provider.CredentialSpec{Kind: "cookie", Label: "Cookies", Help: "copy cookie", Placeholder: "cookie", Links: []provider.CredentialLink{{Label: "console", URL: "https://example.test"}}}
}
func (m *cookieMockProvider) FetchUsage(ctx context.Context, credential, keyType string) (*provider.Usage, error) {
	m.credential = credential
	return m.mockProvider.FetchUsage(ctx, credential, keyType)
}

func (m *mockProvider) ID() string            { return m.id }
func (m *mockProvider) DisplayName() string   { return m.display }
func (m *mockProvider) Aliases() []string     { return m.aliases }
func (m *mockProvider) KeyPrefixes() []string { return m.prefixes }
func (m *mockProvider) KeyPattern() string    { return m.keyPattern }
func (m *mockProvider) ConsoleURL() string    { return m.console }
func (m *mockProvider) UsageConsoleURL(keyType string) string {
	return m.consoleUsage
}
func (m *mockProvider) UsageNote() string { return m.note }
func (m *mockProvider) PlanMeta() provider.PlanMeta {
	return m.meta
}
func (m *mockProvider) UsageType() string {
	if m.usageType != "" {
		return m.usageType
	}
	return provider.UsageTypeBalance
}

func (m *mockProvider) FetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if m.calls != nil {
		m.calls.Add(1)
	}
	var usage *provider.Usage
	var err error
	switch keyType {
	case provider.UsageTypeBalance:
		usage, err = m.balanceUsage, m.balanceErr
	case provider.UsageTypePlan:
		usage, err = m.planUsage, m.planErr
	default:
		return nil, provider.ErrNotSupported
	}
	if err != nil {
		return nil, err
	}
	if usage == nil {
		return nil, provider.ErrNotSupported
	}
	return usage, nil
}

func newMock(id, display string, prefixes []string, usage *provider.Usage, err error) *mockProvider {
	return &mockProvider{
		id:           id,
		display:      display,
		aliases:      []string{id},
		prefixes:     prefixes,
		console:      "https://" + id + ".example",
		consoleUsage: "https://" + id + ".example/usage",
		meta:         provider.PlanMeta{PlanName: id + " Plan", PlanPrice: "官方订阅"},
		usageType:    provider.UsageTypeBalance,
		balanceUsage: usage,
		balanceErr:   err,
		calls:        &atomic.Int64{},
	}
}

// balanceUsage 是 balance 类型的正常用量。
func balanceUsage() *provider.Usage {
	return &provider.Usage{
		BalanceType: "balance",
		Balance:     &provider.Money{Amount: "12.34", Currency: "CNY"},
		UpdatedAt:   time.Now(),
	}
}

// planUsage 是 plan 类型的正常用量（套餐等级 + 配额窗口，无余额）。
func planUsage() *provider.Usage {
	return &provider.Usage{
		BalanceType: "quota",
		Plan:        &provider.PlanInfo{Level: "LEVEL_INTERMEDIATE"},
		Quotas: []provider.Quota{
			{Period: "weekly", Used: "100", Limit: "1000", Remaining: "900"},
		},
		UpdatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newTestServer 构造测试 server。lookups 注入 provider 查找表（含 alias 匹配）。
func newTestServer(t *testing.T, lookups map[string]provider.Provider, appConfigs ...appconf.Config) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute, appConfigs...)
	s.Lookup = func(id string) (provider.Provider, bool) {
		if p, ok := lookups[id]; ok {
			return p, true
		}
		for _, p := range lookups {
			for _, a := range p.Aliases() {
				if a == id {
					return p, true
				}
			}
		}
		return nil, false
	}
	return s, st
}

// doJSON 发一个 HTTP 请求到 server 路由。
func doJSON(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// addTestKey 直接经 store 插入一条默认 both 类型的测试 key。
func addTestKey(t *testing.T, st *store.Store, p, key, account, note string) int64 {
	return addTestKeyType(t, st, p, key, store.KeyTypeBoth, account, note)
}

// addTestKeyType 直接经 store 插入一条指定 key_type 的测试 key。
func addTestKeyType(t *testing.T, st *store.Store, p, key, keyType, account, note string) int64 {
	t.Helper()
	inserted, err := st.AddKey(p, key, keyType, account, note)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if !inserted {
		t.Fatalf("AddKey: 未插入（重复?）")
	}
	recs, err := st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	for _, rec := range recs {
		if rec.Provider == p && rec.Key == key {
			return rec.ID
		}
	}
	t.Fatalf("插入后未找到记录")
	return 0
}

func TestDashboardDisabledRoutesAndReadOnlyKeys(t *testing.T) {
	cfg := appconf.Default()
	cfg.Dashboard.Enabled = false
	s, st := newTestServer(t, nil, cfg)
	id := addTestKey(t, st, "unknown", "sk-dashboard-disabled", "", "")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/usage"},
		{http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id)},
		{http.MethodPost, fmt.Sprintf("/api/keys/%d/usage", id)},
		{http.MethodPost, "/api/refresh"},
	} {
		w := doJSON(t, s, tc.method, tc.path, "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "用量仪表盘功能已在配置文件中禁用") {
			t.Errorf("%s %s: status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/keys"},
		{http.MethodGet, "/api/credentials?provider=unknown"},
	} {
		if w := doJSON(t, s, tc.method, tc.path, ""); w.Code != http.StatusOK {
			t.Errorf("read-only %s %s status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestKeysDisabledWriteRoutesAndDashboardStillWorks(t *testing.T) {
	cfg := appconf.Default()
	cfg.Keys.Enabled = false
	mock := newMock("keys-enabled", "Keys Enabled", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock}, cfg)
	id := addTestKey(t, st, mock.ID(), "sk-keys-disabled", "", "")

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/keys", `{"provider":"x","key":"sk-x"}`},
		{http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"note":"x"}`},
		{http.MethodPut, "/api/keys/reorder", fmt.Sprintf(`{"ids":[%d]}`, id)},
		{http.MethodDelete, fmt.Sprintf("/api/keys/%d", id), ""},
		{http.MethodPost, "/api/credentials", `{"provider":"x","credential":"x"}`},
		{http.MethodDelete, "/api/credentials?provider=x", ""},
	} {
		w := doJSON(t, s, tc.method, tc.path, tc.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Key 管理功能已在配置文件中禁用") {
			t.Errorf("%s %s: status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/keys"},
		{http.MethodGet, "/api/credentials?provider=x"},
		{http.MethodGet, "/api/usage"},
	} {
		if w := doJSON(t, s, tc.method, tc.path, ""); w.Code != http.StatusOK {
			t.Errorf("read/dashboard %s %s status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCredentialRoutesAndUsageCredential(t *testing.T) {
	base := newMock("mimo-cookie-test", "MiMo", nil, balanceUsage(), nil)
	mock := &cookieMockProvider{mockProvider: base}
	s, st := newTestServer(t, map[string]provider.Provider{"mimo-cookie-test": mock})
	keyID := addTestKeyType(t, st, "mimo-cookie-test", "tp-secret", store.KeyTypeBalance, "", "")

	if got := doJSON(t, s, "POST", "/api/credentials", `{"provider":"mimo-cookie-test","account":"","credential":"sid=verysecret; token=abc"}`); got.Code != http.StatusOK {
		t.Fatalf("POST credentials status=%d body=%s", got.Code, got.Body)
	}
	get := doJSON(t, s, "GET", "/api/credentials?provider=mimo-cookie-test&account=", "")
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "verysecret") {
		t.Fatalf("GET cookies leaked or failed: %d %s", get.Code, get.Body)
	}
	var credentialView credentialResponse
	if err := json.Unmarshal(get.Body.Bytes(), &credentialView); err != nil {
		t.Fatal(err)
	}
	if !credentialView.HasCredential || credentialView.Masked == "" || credentialView.UpdatedAt == "" {
		t.Fatalf("credential view = %+v", credentialView)
	}
	usage := doJSON(t, s, "GET", "/api/usage", "")
	if usage.Code != http.StatusOK {
		t.Fatalf("usage status=%d", usage.Code)
	}
	var usageResponse allUsageResponse
	if err := json.Unmarshal(usage.Body.Bytes(), &usageResponse); err != nil {
		t.Fatal(err)
	}
	if len(usageResponse.Results) != 1 || usageResponse.Results[0].Credential == nil || usageResponse.Results[0].Credential.Kind != "cookie" {
		t.Fatalf("credential metadata missing: %+v", usageResponse.Results)
	}
	single := doJSON(t, s, "GET", fmt.Sprintf("/api/keys/%d/usage", keyID), "")
	var singleResponse singleUsageResponse
	if err := json.Unmarshal(single.Body.Bytes(), &singleResponse); err != nil {
		t.Fatal(err)
	}
	if singleResponse.Credential == nil || singleResponse.Credential.Kind != "cookie" {
		t.Fatalf("single usage credential metadata missing: %+v", singleResponse)
	}
	if mock.credential != "sid=verysecret; token=abc" {
		t.Fatalf("fetch credential = %q", mock.credential)
	}
	if got := doJSON(t, s, "POST", "/api/credentials", `{"provider":"mimo-cookie-test","account":"","credential":"sid=replaced"}`); got.Code != http.StatusOK {
		t.Fatalf("credential update status=%d", got.Code)
	}
	doJSON(t, s, "GET", "/api/usage", "")
	if mock.credential != "sid=replaced" || mock.calls.Load() != 2 {
		t.Fatalf("cache was not invalidated after update: credential=%q calls=%d", mock.credential, mock.calls.Load())
	}

	if got := doJSON(t, s, "DELETE", "/api/credentials?provider=mimo-cookie-test&account=", ""); got.Code != http.StatusOK {
		t.Fatalf("DELETE credentials status=%d body=%s", got.Code, got.Body)
	}
	if _, err := st.GetAccountCredential("mimo-cookie-test", ""); err == nil {
		t.Fatal("credential still exists after delete")
	}
	missing := doJSON(t, s, "GET", "/api/usage", "")
	var missingResponse allUsageResponse
	if err := json.Unmarshal(missing.Body.Bytes(), &missingResponse); err != nil {
		t.Fatal(err)
	}
	if len(missingResponse.Results) != 1 || missingResponse.Results[0].Usage == nil || missingResponse.Results[0].Usage.ErrorCode != "credential_missing" {
		t.Fatalf("cached credential was not invalidated after delete: %+v", missingResponse.Results)
	}
}

type fallbackCredentialMock struct {
	*mockProvider
	credential string
}

func (m *fallbackCredentialMock) CredentialSpec() *provider.CredentialSpec {
	return &provider.CredentialSpec{Kind: "token", Label: "Token", KeyFallback: true}
}
func (m *fallbackCredentialMock) FetchUsage(ctx context.Context, credential, keyType string) (*provider.Usage, error) {
	m.credential = credential
	return m.mockProvider.FetchUsage(ctx, credential, keyType)
}

func TestCredentialKeyFallbackAndMetadataOmit(t *testing.T) {
	fallback := &fallbackCredentialMock{mockProvider: newMock("credential-fallback", "Fallback", nil, balanceUsage(), nil)}
	s, st := newTestServer(t, map[string]provider.Provider{fallback.ID(): fallback})
	addTestKeyType(t, st, fallback.ID(), "eyJ.legacy", store.KeyTypeBalance, "account", "")
	w := doJSON(t, s, "GET", "/api/usage", "")
	var response allUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if fallback.credential != "eyJ.legacy" {
		t.Fatalf("fallback credential = %q", fallback.credential)
	}
	if len(response.Results) != 1 || response.Results[0].Credential == nil || response.Results[0].Credential.Kind != "token" {
		t.Fatalf("credential metadata missing: %+v", response.Results)
	}

	keyProvider := newMock("key-only", "Key Only", nil, balanceUsage(), nil)
	s2, st2 := newTestServer(t, map[string]provider.Provider{keyProvider.ID(): keyProvider})
	addTestKeyType(t, st2, keyProvider.ID(), "sk-key", store.KeyTypeBalance, "", "")
	w = doJSON(t, s2, "GET", "/api/usage", "")
	if strings.Contains(w.Body.String(), `"credential"`) {
		t.Fatalf("key provider response has credential metadata: %s", w.Body)
	}
}

func TestCookieMissingUsageIsLocalResult(t *testing.T) {
	mock := &cookieMockProvider{mockProvider: newMock("mimo-missing-test", "MiMo", nil, balanceUsage(), nil)}
	s, st := newTestServer(t, map[string]provider.Provider{"mimo-missing-test": mock})
	addTestKeyType(t, st, "mimo-missing-test", "tp-secret", store.KeyTypeBalance, "acct", "")
	w := doJSON(t, s, "GET", "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var response allUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Usage == nil || response.Results[0].Usage.ErrorCode != "credential_missing" {
		t.Fatalf("results = %+v", response.Results)
	}
	if mock.credential != "" {
		t.Fatalf("missing cookie called provider with %q", mock.credential)
	}
}

type expiredCredentialMock struct{ *mockProvider }

func (m *expiredCredentialMock) CredentialSpec() *provider.CredentialSpec {
	return &provider.CredentialSpec{Kind: "token", Label: "Token"}
}
func (m *expiredCredentialMock) FetchUsage(context.Context, string, string) (*provider.Usage, error) {
	return &provider.Usage{ErrorCode: "credential_expired", Error: "测试凭证已失效"}, nil
}

func TestCredentialExpiredIsLocalUsage(t *testing.T) {
	mock := &expiredCredentialMock{mockProvider: newMock("expired-credential-test", "Expired", nil, nil, nil)}
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	if err := st.SetAccountCredential(mock.ID(), "acct", "expired-token"); err != nil {
		t.Fatal(err)
	}
	addTestKeyType(t, st, mock.ID(), "legacy-key", store.KeyTypeBalance, "acct", "")
	w := doJSON(t, s, "GET", "/api/usage", "")
	var response allUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Usage == nil || response.Results[0].Usage.ErrorCode != "credential_expired" {
		t.Fatalf("results = %+v", response.Results)
	}
}

func decodeResp(t *testing.T, w *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET /api/keys
// ---------------------------------------------------------------------------

func TestListKeys(t *testing.T) {
	mock := newMock("mock-a", "Mock A", []string{"sk-"}, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})

	key := "sk-abcdef12345678"
	addTestKey(t, st, "mock-a", key, "主key", "说明")
	addTestKey(t, st, "ghost", "ghost-key-12345", "", "") // 不在 registry 的 provider

	w := doJSON(t, s, http.MethodGet, "/api/keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var resp []keyResponse
	decodeResp(t, w, &resp)
	if len(resp) != 2 {
		t.Fatalf("len = %d, want 2", len(resp))
	}

	// mock-a：脱敏 + 完整元数据
	a := resp[0]
	if a.Provider != "mock-a" || a.ProviderDisplayName != "Mock A" {
		t.Errorf("provider 字段异常: %+v", a)
	}
	wantMasked := provider.MaskKey(key)
	if a.KeyMasked != wantMasked {
		t.Errorf("key_masked = %q, want %q", a.KeyMasked, wantMasked)
	}
	if strings.Contains(w.Body.String(), key) {
		t.Errorf("响应包含明文 key: %s", key)
	}
	if a.ConsoleURL != "https://mock-a.example" {
		t.Errorf("console_url = %q", a.ConsoleURL)
	}
	if a.PlanMeta.PlanName != "mock-a Plan" {
		t.Errorf("plan_meta.plan_name = %q", a.PlanMeta.PlanName)
	}
	if a.Account != "主key" || a.Note != "说明" || a.CreatedAt == "" {
		t.Errorf("account/note/created_at 异常: %+v", a)
	}

	// ghost：不在 registry → console_url/display_name 空
	g := resp[1]
	if g.Provider != "ghost" || g.ConsoleURL != "" || g.ProviderDisplayName != "" {
		t.Errorf("未注册 provider 字段应为空: %+v", g)
	}
	if g.KeyMasked != provider.MaskKey("ghost-key-12345") {
		t.Errorf("ghost key_masked = %q", g.KeyMasked)
	}
}

// ---------------------------------------------------------------------------
// POST /api/keys
// ---------------------------------------------------------------------------

func TestAddKey(t *testing.T) {
	mock := newMock("mock-a", "Mock A", []string{"sk-"}, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})

	// 成功
	w := doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-a","key":"sk-abc123456789","account":"新key","note":"备注"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var added keyResponse
	decodeResp(t, w, &added)
	if added.ID == 0 || added.KeyMasked != provider.MaskKey("sk-abc123456789") {
		t.Errorf("响应数据异常: %+v", added)
	}
	if added.Account != "新key" || added.Note != "备注" {
		t.Errorf("account/note 异常: %+v", added)
	}
	if added.Provider != "mock-a" {
		t.Errorf("provider = %q, want mock-a", added.Provider)
	}
	// 已写入 store
	if _, err := st.GetKey(added.ID); err != nil {
		t.Errorf("GetKey(%d): %v", added.ID, err)
	}

	// 未知 provider → 201（fallback：以输入值原样存储，跳过前缀校验）
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"nope","key":"sk-xxx"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("未知 provider status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var unknown keyResponse
	decodeResp(t, w, &unknown)
	if unknown.Provider != "nope" || unknown.ProviderDisplayName != "" {
		t.Errorf("未知 provider 响应错误（应存输入名、display 空）: %+v", unknown)
	}

	// 通过 provider 别名（aliases）添加 → 201
	mock.aliases = append(mock.aliases, "mock-alias")
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-alias","key":"sk-alias-key-99"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("provider 别名添加 status = %d, want 201, body=%s", w.Code, w.Body.String())
	}

	// key 空 → 400
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-a","key":"   "}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("空 key status = %d, want 400", w.Code)
	}

	// 前缀不匹配 → 400
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-a","key":"bad-key"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("前缀不匹配 status = %d, want 400, body=%s", w.Code, w.Body.String())
	}

	// KeyPattern 正则校验：mock-b 声明 32hex.16alnum 正则。
	mockB := newMock("mock-b", "Mock B", nil, balanceUsage(), nil)
	mockB.keyPattern = `^[0-9a-f]{32}\.[A-Za-z0-9]{16}$`
	s2, _ := newTestServer(t, map[string]provider.Provider{mockB.ID(): mockB})
	// 合法格式 → 201
	zaiKey := "91982d3f89d84389a7428f3da81f4621.s9IJLXCUWsyjY6ja"
	w = doJSON(t, s2, http.MethodPost, "/api/keys", `{"provider":"mock-b","key":"`+zaiKey+`"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("正则匹配 key status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	// 非法格式（sk- 前缀）→ 400
	w = doJSON(t, s2, http.MethodPost, "/api/keys", `{"provider":"mock-b","key":"sk-abc123"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("正则不匹配 status = %d, want 400, body=%s", w.Code, w.Body.String())
	}

	// 重复添加 → 409
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-a","key":"sk-abc123456789"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("重复 status = %d, want 409", w.Code)
	}

	// 非法 JSON → 400
	w = doJSON(t, s, http.MethodPost, "/api/keys", "not json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON status = %d, want 400", w.Code)
	}

	// 错误响应统一 {"error":...}
	w = doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"","key":"x"}`)
	var errResp map[string]string
	decodeResp(t, w, &errResp)
	if errResp["error"] == "" {
		t.Errorf("错误响应缺少 error 字段: %s", w.Body.String())
	}
}

func TestAddKeyNoPrefixValidation(t *testing.T) {
	// KeyPrefixes 返回 nil → 不校验前缀（fallback 语义）
	mock := newMock("mock-free", "Mock Free", nil, balanceUsage(), nil)
	s, _ := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})

	w := doJSON(t, s, http.MethodPost, "/api/keys", `{"provider":"mock-free","key":"anything-here"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (无前缀校验), body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/keys/{id}
// ---------------------------------------------------------------------------

func TestDeleteKey(t *testing.T) {
	s, st := newTestServer(t, nil)
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodDelete, fmt.Sprintf("/api/keys/%d", id), "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", w.Code, w.Body.String())
	}
	if _, err := st.GetKey(id); err == nil {
		t.Errorf("删除后 GetKey 仍成功")
	}

	// 删除后再次删除 → 404
	w = doJSON(t, s, http.MethodDelete, fmt.Sprintf("/api/keys/%d", id), "")
	if w.Code != http.StatusNotFound {
		t.Errorf("二次删除 status = %d, want 404", w.Code)
	}

	// 不存在 → 404
	w = doJSON(t, s, http.MethodDelete, "/api/keys/9999", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在 status = %d, want 404", w.Code)
	}

	// 非法 id → 400
	w = doJSON(t, s, http.MethodDelete, "/api/keys/abc", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 id status = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// PATCH /api/keys/{id}
// ---------------------------------------------------------------------------

func TestPatchKey(t *testing.T) {
	s, st := newTestServer(t, nil)
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "旧账户", "旧note")

	// 只更新 account（note 保留）
	w := doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"account":"新账户"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp keyResponse
	decodeResp(t, w, &resp)
	if resp.Account != "新账户" || resp.Note != "旧note" {
		t.Errorf("部分更新异常: account=%q note=%q", resp.Account, resp.Note)
	}

	// 只更新 note
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"note":"新note"}`)
	resp = keyResponse{} // Unmarshal 不重置已有字段，复用前清空
	decodeResp(t, w, &resp)
	if resp.Account != "新账户" || resp.Note != "新note" {
		t.Errorf("note 更新异常: account=%q note=%q", resp.Account, resp.Note)
	}

	// 显式置空 account
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"account":""}`)
	resp = keyResponse{}
	decodeResp(t, w, &resp)
	if resp.Account != "" || resp.Note != "新note" {
		t.Errorf("置空结果异常: account=%q note=%q", resp.Account, resp.Note)
	}

	// 空 body → 200 无操作
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), "")
	if w.Code != http.StatusOK {
		t.Errorf("空 body status = %d, want 200", w.Code)
	}

	// 不存在 → 404
	w = doJSON(t, s, http.MethodPatch, "/api/keys/9999", `{"account":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在 status = %d, want 404", w.Code)
	}

	// 非法 id → 400
	w = doJSON(t, s, http.MethodPatch, "/api/keys/abc", `{"account":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 id status = %d, want 400", w.Code)
	}
}

// TestPatchKeyAccountAndKey 覆盖 T36 PATCH 语义：account（账户）变更、key 空串保持原值、key 变更。
func TestPatchKeyAccountAndKey(t *testing.T) {
	s, st := newTestServer(t, nil)
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "旧账户", "旧note")

	// 改 account（账户）
	w := doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"account":"主账户"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("account 更新 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp keyResponse
	decodeResp(t, w, &resp)
	if resp.Account != "主账户" {
		t.Errorf("account = %q, want 主账户", resp.Account)
	}

	// key 提供空串/纯空格 → 保持原 key
	for _, body := range []string{`{"key":""}`, `{"key":"   "}`} {
		w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), body)
		if w.Code != http.StatusOK {
			t.Fatalf("空 key PATCH status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		resp = keyResponse{}
		decodeResp(t, w, &resp)
		if resp.KeyMasked != provider.MaskKey("sk-abc123456789") {
			t.Errorf("空 key 应保持原 key，got %q", resp.KeyMasked)
		}
	}

	// key 变更
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"key":"sk-newkey-123456"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("key 变更 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	resp = keyResponse{}
	decodeResp(t, w, &resp)
	if resp.KeyMasked != provider.MaskKey("sk-newkey-123456") {
		t.Errorf("key 变更后 key_masked = %q", resp.KeyMasked)
	}
	// store 已持久化新 key
	rec, err := st.GetKey(id)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if rec.Key != "sk-newkey-123456" || rec.Account != "主账户" {
		t.Errorf("store 未持久化变更: %+v", rec)
	}
}

// TestPatchKeyProviderChange 覆盖 provider 变更：已知 provider 前缀校验、未知 provider fallback。
func TestPatchKeyProviderChange(t *testing.T) {
	mockA := newMock("mock-a", "Mock A", []string{"sk-"}, balanceUsage(), nil)
	mockB := newMock("mock-b", "Mock B", []string{"sb-"}, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mockA.ID(): mockA, mockB.ID(): mockB})
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "", "")

	// 换到 mock-b 但 key 前缀不匹配 → 400
	w := doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"provider":"mock-b"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("前缀不匹配 status = %d, want 400, body=%s", w.Code, w.Body.String())
	}

	// key 一起换到匹配前缀 → 200
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"provider":"mock-b","key":"sb-newkey-123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("provider 变更 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp keyResponse
	decodeResp(t, w, &resp)
	if resp.Provider != "mock-b" || resp.ProviderDisplayName != "Mock B" {
		t.Errorf("provider 变更响应异常: %+v", resp)
	}
	if resp.KeyMasked != provider.MaskKey("sb-newkey-123") {
		t.Errorf("key_masked = %q", resp.KeyMasked)
	}

	// 未知 provider（不在 registry）→ fallback 存输入值，跳过前缀校验
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"provider":"ghost","key":"ghost-key-9"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("未知 provider 变更 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	resp = keyResponse{}
	decodeResp(t, w, &resp)
	if resp.Provider != "ghost" {
		t.Errorf("未知 provider 应存输入值, got %q", resp.Provider)
	}

	// provider 提供空串 → 400
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), `{"provider":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("空 provider status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestPatchKeyConflict 覆盖 (provider, key) 与其他记录冲突 → 409，自身不冲突。
func TestPatchKeyConflict(t *testing.T) {
	s, st := newTestServer(t, nil)
	id1 := addTestKey(t, st, "mock-a", "sk-key-111111", "", "")
	addTestKey(t, st, "mock-a", "sk-key-222222", "", "")

	// key1 改成 key2 的 key → (mock-a, sk-key-222222) 冲突 → 409
	w := doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id1), `{"key":"sk-key-222222"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("冲突 status = %d, want 409, body=%s", w.Code, w.Body.String())
	}

	// 不改动（自身组合）→ 200
	w = doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id1), `{"account":"x"}`)
	if w.Code != http.StatusOK {
		t.Errorf("自身组合 status = %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/keys/{id}/usage
// ---------------------------------------------------------------------------

func TestKeyUsage(t *testing.T) {
	// 正常
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "我的别名", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.KeyID != id || resp.Provider != "mock-a" {
		t.Errorf("key_id/provider 异常: %+v", resp)
	}
	if resp.Account != "我的别名" {
		t.Errorf("account = %q, want 我的别名（透传 key 别名）", resp.Account)
	}
	if resp.Usage == nil || resp.Usage.BalanceType != "balance" || resp.Usage.Balance.Amount != "12.34" {
		t.Errorf("usage 异常: %+v", resp.Usage)
	}
	if resp.Error != "" || resp.ConsoleURL != "https://mock-a.example" {
		t.Errorf("error/console_url 异常: %+v", resp)
	}
}

func TestKeyUsageNotSupported(t *testing.T) {
	// 仅存储 provider（mimo）：both key 两个接口都 ErrNotSupported → usage.error="类型不匹配"
	mock := newMock("mimo", "Xiaomi MiMo", nil, nil, provider.ErrNotSupported)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mimo", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Usage == nil || !strings.Contains(resp.Usage.Error, "类型不匹配") {
		t.Errorf("usage.error = %+v, want 含「类型不匹配」", resp.Usage)
	}
	if resp.Error != "" {
		t.Errorf("顶层 error = %q, want 空（类型不匹配走 usage.error）", resp.Error)
	}
	if resp.ConsoleURL != "https://mimo.example" {
		t.Errorf("console_url = %q", resp.ConsoleURL)
	}
}

func TestKeyUsageFetchError(t *testing.T) {
	mock := newMock("mock-err", "Mock Err", nil, nil, errors.New("upstream boom"))
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKeyType(t, st, "mock-err", "sk-abc123456789", store.KeyTypeBalance, "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (局部失败), body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Error != "上游暂不可用，请稍后重试" {
		t.Errorf("error = %q", resp.Error)
	}
	if resp.KeyID != id {
		t.Errorf("key_id = %d, want %d", resp.KeyID, id)
	}
	if resp.Usage != nil {
		t.Errorf("fetch 错误时不应有 usage")
	}
}

func TestUsageErrorMessagesAreLocalized(t *testing.T) {
	cases := []struct {
		id, keyType string
		err         error
		want        string
	}{
		{"deepseek", store.KeyTypeBalance, fmt.Errorf("%w: HTTP 401", provider.ErrAuth), "DeepSeek 认证失败（HTTP 401），请检查 Key 是否有效"},
		{"opencode-go", store.KeyTypePlan, fmt.Errorf("%w: HTTP 403", provider.ErrUpstream), "OpenCode Go 拒绝访问（HTTP 403），请检查套餐或权限条件"},
		{"minimax", store.KeyTypePlan, fmt.Errorf("%w: HTTP 503", provider.ErrUpstream), "MiniMax 上游暂不可用（HTTP 503），请稍后重试"},
		{"kimi-code", store.KeyTypePlan, provider.ErrRateLimit, "请求被上游限流，请稍后重试"},
		{"deepseek", store.KeyTypeBalance, provider.ErrParse, "DeepSeek 上游响应异常，无法解析余额数据"},
		{"mock", store.KeyTypeBalance, context.DeadlineExceeded, "网络连接失败，请检查网络后重试"},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.want, func(t *testing.T) {
			p := newMock(tc.id, tc.id, nil, nil, nil)
			if got := usageErrorMessage(p, tc.keyType, tc.err); got != tc.want {
				t.Errorf("usageErrorMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyUsageNotFound(t *testing.T) {
	s, _ := newTestServer(t, nil)

	w := doJSON(t, s, http.MethodGet, "/api/keys/9999/usage", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在 status = %d, want 404", w.Code)
	}
	w = doJSON(t, s, http.MethodGet, "/api/keys/abc/usage", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 id status = %d, want 400", w.Code)
	}
}

func TestKeyUsageUnknownProvider(t *testing.T) {
	// provider 未注册 → 200 + error
	s, st := newTestServer(t, nil)
	id := addTestKey(t, st, "ghost", "ghost-key-12345", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Error == "" {
		t.Errorf("provider 未注册应带 error")
	}
}

// ---------------------------------------------------------------------------
// POST /api/keys/{id}/usage（单 key 强制刷新）
// ---------------------------------------------------------------------------

func TestKeyUsageCarriesConsoleUsageURL(t *testing.T) {
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	mock.note = "测试用量说明"
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mock-a", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.ConsoleUsageURL != "https://mock-a.example/usage" {
		t.Errorf("console_usage_url = %q, want mock 用量页", resp.ConsoleUsageURL)
	}
	if resp.UsageNote != "测试用量说明" {
		t.Errorf("usage_note = %q, want mock 说明文案", resp.UsageNote)
	}
}

func TestRefreshKeyUsageForceRefetch(t *testing.T) {
	// 缓存命中时 GET 不打上游；POST 强制刷新必须重新 fetch。
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKeyType(t, st, "mock-a", "sk-abc123456789", store.KeyTypeBalance, "", "")

	if w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), ""); w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", w.Code, w.Body.String())
	}
	before := mock.calls.Load()

	// 缓存未过期 → GET 不再 fetch
	if w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), ""); w.Code != http.StatusOK {
		t.Fatalf("GET 2 status = %d", w.Code)
	}
	if mock.calls.Load() != before {
		t.Errorf("缓存命中时不应再次 fetch: calls %d → %d", before, mock.calls.Load())
	}

	// POST 强制刷新 → 必须重新 fetch
	w := doJSON(t, s, http.MethodPost, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Usage == nil {
		t.Errorf("强制刷新应返回 usage")
	}
	if mock.calls.Load() != before+1 {
		t.Errorf("POST 应强制重新 fetch: calls %d → %d, want %d", before, mock.calls.Load(), before+1)
	}
}

func TestRefreshKeyUsageSyncsSiblingCache(t *testing.T) {
	// 同 (provider, account, keyType) 的兄弟 key 共享账户数据：
	// POST 刷新代表 key 后，兄弟 key 的缓存被同步写入，后续 GET 不再打上游。
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	repID := addTestKeyType(t, st, "mock-a", "sk-rep-123456789", store.KeyTypeBalance, "共享账户", "")
	sibID := addTestKeyType(t, st, "mock-a", "sk-sib-987654321", store.KeyTypeBalance, "共享账户", "")

	if w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", repID), ""); w.Code != http.StatusOK {
		t.Fatalf("GET rep status = %d", w.Code)
	}
	before := mock.calls.Load()

	// POST 刷新代表 key（此时会 fetch 一次，并同步写兄弟缓存）
	if w := doJSON(t, s, http.MethodPost, fmt.Sprintf("/api/keys/%d/usage", repID), ""); w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", w.Code, w.Body.String())
	}
	afterRefresh := mock.calls.Load()
	if afterRefresh != before+1 {
		t.Errorf("POST 应 fetch 1 次: %d → %d", before, afterRefresh)
	}

	// 兄弟 key GET：命中同步写入的缓存，不再打上游
	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", sibID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET sibling status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Usage == nil || resp.Usage.Balance.Amount != "12.34" {
		t.Errorf("兄弟 key 未命中同步缓存: %+v", resp.Usage)
	}
	if mock.calls.Load() != afterRefresh {
		t.Errorf("兄弟 key 应命中缓存不再 fetch: calls %d → %d", afterRefresh, mock.calls.Load())
	}
}

func TestRefreshKeyUsageErrors(t *testing.T) {
	s, _ := newTestServer(t, nil)

	if w := doJSON(t, s, http.MethodPost, "/api/keys/9999/usage", ""); w.Code != http.StatusNotFound {
		t.Errorf("不存在 status = %d, want 404", w.Code)
	}
	if w := doJSON(t, s, http.MethodPost, "/api/keys/abc/usage", ""); w.Code != http.StatusBadRequest {
		t.Errorf("非法 id status = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// key_type 调度（both 合并 / 类型不匹配 / 单类型）
// ---------------------------------------------------------------------------

func TestKeyUsageBothMergesBalanceAndPlan(t *testing.T) {
	// both key + provider 双类型都支持 → 合并 Balance + Quotas + Plan，usage_type="both"
	mock := newMock("mock-both", "Mock Both", nil, nil, nil)
	mock.balanceUsage = balanceUsage()
	mock.planUsage = planUsage()
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mock-both", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.UsageType != store.KeyTypeBoth {
		t.Errorf("usage_type = %q, want both", resp.UsageType)
	}
	if resp.Usage == nil {
		t.Fatal("usage 应为非空")
	}
	if resp.Usage.Balance == nil || resp.Usage.Balance.Amount != "12.34" {
		t.Errorf("balance 未合并: %+v", resp.Usage.Balance)
	}
	if resp.Usage.Plan == nil || resp.Usage.Plan.Level != "LEVEL_INTERMEDIATE" {
		t.Errorf("plan 未合并: %+v", resp.Usage.Plan)
	}
	if len(resp.Usage.Quotas) != 1 || resp.Usage.Quotas[0].Period != "weekly" {
		t.Errorf("quotas 未合并: %+v", resp.Usage.Quotas)
	}
	if resp.Error != "" {
		t.Errorf("不应有顶层 error: %q", resp.Error)
	}
}

func TestKeyUsageBothOnlyBalance(t *testing.T) {
	// both key + provider 只支持 balance → 只返回余额，usage_type="balance"
	mock := newMock("mock-bal", "Mock Bal", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mock-bal", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.UsageType != provider.UsageTypeBalance {
		t.Errorf("usage_type = %q, want balance", resp.UsageType)
	}
	if resp.Usage == nil || resp.Usage.Balance == nil {
		t.Errorf("应返回余额: %+v", resp.Usage)
	}
	if resp.Usage.Plan != nil {
		t.Errorf("plan 不应存在: %+v", resp.Usage.Plan)
	}
}

func TestKeyUsageBothBalanceOKPlanFails(t *testing.T) {
	// both key：balance 成功 + plan 真实失败 → 返回余额 + 失败信息进 usage.error
	mock := newMock("mock-both", "Mock Both", nil, nil, nil)
	mock.balanceUsage = balanceUsage()
	mock.planErr = errors.New("plan upstream down")
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKey(t, st, "mock-both", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.UsageType != provider.UsageTypeBalance {
		t.Errorf("usage_type = %q, want balance", resp.UsageType)
	}
	if resp.Usage == nil || resp.Usage.Balance == nil {
		t.Errorf("应返回余额: %+v", resp.Usage)
	}
	if resp.Usage.Error != "套餐查询失败：上游暂不可用，请稍后重试" {
		t.Errorf("usage.error = %q", resp.Usage.Error)
	}
}

func TestKeyUsageTypeMismatch(t *testing.T) {
	// plan key + 只支持 balance 的 provider → 类型不匹配
	mock := newMock("mock-bal", "Mock Bal", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKeyType(t, st, "mock-bal", "sk-abc123456789", store.KeyTypePlan, "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Usage == nil || !strings.Contains(resp.Usage.Error, "类型不匹配") {
		t.Errorf("应返回类型不匹配: %+v", resp.Usage)
	}
	// T44 实际情况报错：plan 通道无实现 → 按 kt 报「不支持套餐查询」。
	if resp.Usage != nil && !strings.Contains(resp.Usage.Error, "不支持套餐查询") {
		t.Errorf("usage.error = %q, want 含「不支持套餐查询」（按 kt 报错）", resp.Usage.Error)
	}
	if resp.UsageType != store.KeyTypePlan {
		t.Errorf("usage_type = %q, want plan", resp.UsageType)
	}
	if resp.Usage.Balance != nil {
		t.Errorf("类型不匹配时不应有 balance 数据")
	}
}

// TestKeyUsageStoreOnlySingleType 守护 T44 统一报错：仅存储平台（两通道均
// ErrNotSupported，如 bailian/mimo/modelscope）与真实能力错位是同一事实，
// 消息一律按 kt 生成「此提供商不支持X查询」，不得出现主类型（UsageType）
// 冒充支持集的「仅支持 Y」表述。
func TestKeyUsageStoreOnlySingleType(t *testing.T) {
	mock := newMock("mock-store-only", "Mock Store Only", nil, nil, provider.ErrNotSupported)
	mock.usageType = provider.UsageTypePlan
	mock.planErr = provider.ErrNotSupported
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKeyType(t, st, "mock-store-only", "sk-storeonly-key1", store.KeyTypePlan, "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.Usage == nil || !strings.Contains(resp.Usage.Error, "类型不匹配") {
		t.Fatalf("应返回类型不匹配: %+v", resp.Usage)
	}
	if !strings.Contains(resp.Usage.Error, "此提供商不支持套餐查询") {
		t.Errorf("usage.error = %q, want 按 kt 报「此提供商不支持套餐查询」", resp.Usage.Error)
	}
	if strings.Contains(resp.Usage.Error, "仅支持") {
		t.Errorf("usage.error = %q, 不得含「仅支持」（UsageType 非能力声明）", resp.Usage.Error)
	}
}

func TestKeyUsageSingleType(t *testing.T) {
	// 单类型 key 只查对应接口：balance 查余额、plan 查套餐，usage_type=标记类型
	mock := newMock("mock-both", "Mock Both", nil, nil, nil)
	mock.balanceUsage = balanceUsage()
	mock.planUsage = planUsage()
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	balID := addTestKeyType(t, st, "mock-both", "sk-bal-123456789", store.KeyTypeBalance, "", "")
	planID := addTestKeyType(t, st, "mock-both", "sk-plan-123456789", store.KeyTypePlan, "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", balID), "")
	var resp singleUsageResponse
	decodeResp(t, w, &resp)
	if resp.UsageType != provider.UsageTypeBalance {
		t.Errorf("balance key usage_type = %q, want balance", resp.UsageType)
	}
	if resp.Usage == nil || resp.Usage.Balance == nil {
		t.Errorf("balance key 应返回余额: %+v", resp.Usage)
	}
	if resp.Usage.Plan != nil {
		t.Errorf("balance key 不应查套餐: %+v", resp.Usage.Plan)
	}

	w = doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", planID), "")
	resp = singleUsageResponse{}
	decodeResp(t, w, &resp)
	if resp.UsageType != provider.UsageTypePlan {
		t.Errorf("plan key usage_type = %q, want plan", resp.UsageType)
	}
	if resp.Usage == nil || resp.Usage.Plan == nil {
		t.Errorf("plan key 应返回套餐: %+v", resp.Usage)
	}
	if resp.Usage.Balance != nil {
		t.Errorf("plan key 不应查余额: %+v", resp.Usage.Balance)
	}
}

func TestAllUsageBothMergesInAggregate(t *testing.T) {
	// 聚合端点与单 key 行为一致：both key 双类型合并
	mock := newMock("mock-both", "Mock Both", nil, nil, nil)
	mock.balanceUsage = balanceUsage()
	mock.planUsage = planUsage()
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKey(t, st, "mock-both", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	r := resp.Results[0]
	if r.UsageType != store.KeyTypeBoth {
		t.Errorf("usage_type = %q, want both", r.UsageType)
	}
	if r.Usage == nil || r.Usage.Balance == nil || r.Usage.Plan == nil {
		t.Errorf("聚合合并失败: %+v", r.Usage)
	}
}

// ---------------------------------------------------------------------------
// GET /api/usage
// ---------------------------------------------------------------------------

func TestAllUsageIsolation(t *testing.T) {
	ok := newMock("mock-ok", "Mock OK", nil, balanceUsage(), nil)
	fail := newMock("mock-fail", "Mock Fail", nil, nil, errors.New("fail boom"))
	s, st := newTestServer(t, map[string]provider.Provider{ok.ID(): ok, fail.ID(): fail})

	okID := addTestKeyType(t, st, "mock-ok", "sk-ok-key-123456", store.KeyTypeBalance, "OK 别名", "")
	failID := addTestKeyType(t, st, "mock-fail", "sk-fail-key-1", store.KeyTypeBalance, "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(resp.Results))
	}
	if resp.UpdatedAt.IsZero() {
		t.Errorf("updated_at 应为非零值")
	}

	// ok 条目：usage 完整 + error 省略
	first := resp.Results[0]
	if first.KeyID != okID || first.Provider != "mock-ok" {
		t.Errorf("ok 条目异常: %+v", first)
	}
	if first.Usage == nil || first.Usage.Balance.Amount != "12.34" {
		t.Errorf("ok usage 异常: %+v", first.Usage)
	}
	if first.Error != "" {
		t.Errorf("ok 条目不应有 error: %q", first.Error)
	}
	if first.ProviderDisplayName != "Mock OK" || first.ConsoleURL != "https://mock-ok.example" {
		t.Errorf("ok 元数据异常: %+v", first)
	}
	if first.Account != "OK 别名" {
		t.Errorf("ok 条目 account = %q, want OK 别名（聚合响应透传 key 别名）", first.Account)
	}

	// fail 条目：error 非空 + usage 省略
	second := resp.Results[1]
	if second.KeyID != failID || second.Provider != "mock-fail" {
		t.Errorf("fail 条目异常: %+v", second)
	}
	if second.Error != "上游暂不可用，请稍后重试" {
		t.Errorf("fail error = %q", second.Error)
	}
	if second.Usage != nil {
		t.Errorf("fail 条目不应有 usage")
	}
}

// TestAllUsageSameAccountFetchesOnce 守护 T36 聚合优化（T37 收紧分组条件后仍成立）：
// 同 provider 同 account（账户）同 key_type 的多个 key 只 fetch 一次，结果复制给组内所有 key。
// 测试要点：
//   - 同账户两个 key（同为 balance 类型）→ mock.calls == 1（只 fetch 一次）
//   - 两条结果的 usage 共享、key_id 保留各自值
//   - account 空的 key 独立请求（另算一次 fetch）
func TestAllUsageSameAccountFetchesOnce(t *testing.T) {
	mock := newMock("mock-acct", "Mock Acct", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})

	idA := addTestKeyType(t, st, "mock-acct", "sk-acct-a-123456", store.KeyTypeBalance, "主账户", "")
	idB := addTestKeyType(t, st, "mock-acct", "sk-acct-b-123456", store.KeyTypeBalance, "主账户", "")
	idC := addTestKeyType(t, st, "mock-acct", "sk-standalone-1", store.KeyTypeBalance, "", "") // account 空 → 独立

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 3 {
		t.Fatalf("results len = %d, want 3", len(resp.Results))
	}

	if n := mock.calls.Load(); n != 2 {
		t.Errorf("FetchUsage calls = %d, want 2（同账户 1 次 + account 空 1 次）", n)
	}

	byID := map[int64]usageResult{}
	for _, r := range resp.Results {
		byID[r.KeyID] = r
	}
	for _, id := range []int64{idA, idB} {
		r := byID[id]
		if r.KeyID != id {
			t.Errorf("key_id = %d, want %d", r.KeyID, id)
		}
		if r.Usage == nil || r.Usage.Balance == nil || r.Usage.Balance.Amount != "12.34" {
			t.Errorf("#%d usage 未共享: %+v", id, r.Usage)
		}
	}
	// 独立 key 也有自己的 usage
	if r := byID[idC]; r.Usage == nil || r.Usage.Balance == nil {
		t.Errorf("#%d 独立 key usage 缺失: %+v", idC, r.Usage)
	}
}

// TestAllUsageSameAccountDifferentTypesFetchesSeparately 守护 T37 分组条件收紧：
// 同账户（同 provider 同 account）但 key_type 不同的 key 不聚合，各自 fetch 一次。
// 测试要点：
//   - balance 类型 key 与 plan 类型 key 同账户 → mock.calls == 2（两种类型各 fetch 一次）
//   - 两条结果的 usage 按各自类型返回（余额 vs 套餐）
func TestAllUsageSameAccountDifferentTypesFetchesSeparately(t *testing.T) {
	mock := newMock("mock-acct", "Mock Acct", nil, balanceUsage(), nil)
	mock.planUsage = planUsage() // 该 provider 同时支持 balance 与 plan 查询
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})

	idBal := addTestKeyType(t, st, "mock-acct", "sk-acct-bal-123456", store.KeyTypeBalance, "主账户", "")
	idPlan := addTestKeyType(t, st, "mock-acct", "sk-acct-plan-123456", store.KeyTypePlan, "主账户", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(resp.Results))
	}

	if n := mock.calls.Load(); n != 2 {
		t.Errorf("FetchUsage calls = %d, want 2（同账户不同类型各自 fetch 一次）", n)
	}

	byID := map[int64]usageResult{}
	for _, r := range resp.Results {
		byID[r.KeyID] = r
	}
	if r := byID[idBal]; r.Usage == nil || r.Usage.Balance == nil || r.Usage.Balance.Amount != "12.34" {
		t.Errorf("#%d balance usage 异常: %+v", idBal, r.Usage)
	}
	if r := byID[idPlan]; r.Usage == nil || r.Usage.Plan == nil || r.Usage.Plan.Level != "LEVEL_INTERMEDIATE" {
		t.Errorf("#%d plan usage 异常: %+v", idPlan, r.Usage)
	}
}

func TestAllUsageNotSupported(t *testing.T) {
	// 仅存储 provider（mimo）在聚合中 → 条目 usage.error="类型不匹配"
	mock := newMock("mimo", "Xiaomi MiMo", nil, nil, provider.ErrNotSupported)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKey(t, st, "mimo", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Usage == nil ||
		!strings.Contains(resp.Results[0].Usage.Error, "类型不匹配") {
		t.Errorf("ErrNotSupported 聚合结果异常: %+v", resp.Results)
	}
	if resp.Results[0].ConsoleURL != "https://mimo.example" {
		t.Errorf("console_url = %q", resp.Results[0].ConsoleURL)
	}
}

func TestAllUsageEmpty(t *testing.T) {
	s, _ := newTestServer(t, nil)

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if resp.Results == nil {
		t.Errorf("空库 results 应为 [] 而非 null")
	}
	if len(resp.Results) != 0 {
		t.Errorf("results len = %d, want 0", len(resp.Results))
	}
}

// ---------------------------------------------------------------------------
// modelscope forceTTL + refresh
// ---------------------------------------------------------------------------

// TestAllUsageModelscopeForceTTL 验证 modelscope 强制 1h 缓存、普通 provider 用默认 TTL。
// 默认 TTL 设 1ms：第二次拉取时普通 provider 已过期重新 fetch，
// modelscope 仍命中 1h 缓存（每次查询烧 1 次额度，learnings T12）。
func TestAllUsageModelscopeForceTTL(t *testing.T) {
	ms := newMock("modelscope", "ModelScope 魔搭", nil, balanceUsage(), nil)
	plain := newMock("mock-plain", "Mock Plain", nil, balanceUsage(), nil)
	lookups := map[string]provider.Provider{ms.ID(): ms, plain.ID(): plain}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// 默认 TTL 1ms，足够让第二次拉取时普通 provider 过期
	s := NewServer(st, cache.New(time.Millisecond), time.Millisecond)
	s.Lookup = func(id string) (provider.Provider, bool) {
		p, ok := lookups[id]
		return p, ok
	}
	addTestKeyType(t, st, "modelscope", "ms-key-123456789", store.KeyTypeBalance, "", "")
	addTestKeyType(t, st, "mock-plain", "sk-plain-123456", store.KeyTypeBalance, "", "")

	doJSON(t, s, http.MethodGet, "/api/usage", "")
	if got := ms.calls.Load(); got != 1 {
		t.Fatalf("modelscope 首次拉取 calls = %d, want 1", got)
	}
	if got := plain.calls.Load(); got != 1 {
		t.Fatalf("plain 首次拉取 calls = %d, want 1", got)
	}

	// 等待默认 TTL(1ms) 过期
	time.Sleep(20 * time.Millisecond)
	doJSON(t, s, http.MethodGet, "/api/usage", "")

	if got := ms.calls.Load(); got != 1 {
		t.Errorf("modelscope 第二次拉取 calls = %d, want 1 (1h 缓存命中)", got)
	}
	if got := plain.calls.Load(); got != 2 {
		t.Errorf("plain 第二次拉取 calls = %d, want 2 (1ms 过期重新 fetch)", got)
	}
}

func TestRefresh(t *testing.T) {
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKeyType(t, st, "mock-a", "sk-abc123456789", store.KeyTypeBalance, "", "")

	// 首次拉取写入缓存
	doJSON(t, s, http.MethodGet, "/api/usage", "")
	if got := mock.calls.Load(); got != 1 {
		t.Fatalf("首次 calls = %d, want 1", got)
	}

	// refresh 清除缓存并重新拉取 → calls 2
	w := doJSON(t, s, http.MethodPost, "/api/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Errorf("results len = %d, want 1", len(resp.Results))
	}
	if got := mock.calls.Load(); got != 2 {
		t.Errorf("refresh 后 calls = %d, want 2", got)
	}

	// 再次 GET 命中缓存 → calls 仍 2
	doJSON(t, s, http.MethodGet, "/api/usage", "")
	if got := mock.calls.Load(); got != 2 {
		t.Errorf("refresh 后 GET calls = %d, want 2 (缓存命中)", got)
	}
}

// ---------------------------------------------------------------------------
// GET /api/keys/{id}/key
// ---------------------------------------------------------------------------

func TestGetFullKey(t *testing.T) {
	s, st := newTestServer(t, nil)
	const fullKey = "sk-fullkey-abcdef1234567890"
	id := addTestKey(t, st, "mock-a", fullKey, "", "")

	w := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/key", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp fullKeyResponse
	decodeResp(t, w, &resp)
	if resp.Key != fullKey {
		t.Errorf("key = %q, want %q", resp.Key, fullKey)
	}
}

func TestGetFullKeyNotFound(t *testing.T) {
	s, _ := newTestServer(t, nil)
	w := doJSON(t, s, http.MethodGet, "/api/keys/9999/key", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在 status = %d, want 404", w.Code)
	}
}

func TestGetFullKeyInvalidID(t *testing.T) {
	s, _ := newTestServer(t, nil)
	w := doJSON(t, s, http.MethodGet, "/api/keys/abc/key", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 id status = %d, want 400", w.Code)
	}
}

// TestUsageResultHasUsageTypeField 验证 usageResult 含 usage_type 字段。
func TestUsageResultHasUsageTypeField(t *testing.T) {
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKey(t, st, "mock-a", "sk-abc123456789", "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	if resp.Results[0].UsageType != provider.UsageTypeBalance {
		t.Errorf("usage_type = %q, want %q", resp.Results[0].UsageType, provider.UsageTypeBalance)
	}
}

func TestUsageResultUsageTypeOnError(t *testing.T) {
	// fetch 失败时 usage_type 仍存在（前端据此决定分类）
	mock := newMock("mock-fail", "Mock Fail", nil, nil, errors.New("boom"))
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKeyType(t, st, "mock-fail", "sk-fail-12345", store.KeyTypeBalance, "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	if resp.Results[0].UsageType != provider.UsageTypeBalance {
		t.Errorf("error 时 usage_type = %q, want %q", resp.Results[0].UsageType, provider.UsageTypeBalance)
	}
	if resp.Results[0].Error == "" {
		t.Errorf("error 应非空")
	}
}

func TestUsageResultSkipUnknownProvider(t *testing.T) {
	// provider 未注册 → 条目标记 skip（前端完全跳过，不显示在任何组）
	s, st := newTestServer(t, nil)
	addTestKey(t, st, "ghost", "ghost-key-12345", "", "")

	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	if !resp.Results[0].Skip {
		t.Errorf("未注册 provider skip = false, want true: %+v", resp.Results[0])
	}
	if resp.Results[0].UsageType != "" || resp.Results[0].Error != "" {
		t.Errorf("skip 条目不应有 usage_type/error: %+v", resp.Results[0])
	}

	// snapshot 的 Usage 恒为空数组：首屏只渲染 key 列表，用量由前端逐卡片拉取
	// （GET/POST /api/keys/{id}/usage），避免任一 provider 卡住拖挂页面渲染。
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snap.Usage) != 0 {
		t.Errorf("snapshot usage 应恒为空: %+v", snap.Usage)
	}
	if len(snap.Keys) != 1 {
		t.Errorf("snapshot keys len = %d, want 1", len(snap.Keys))
	}
}

func TestUsageResultSkipDisabledKey(t *testing.T) {
	// 禁用 key：聚合查询标 skip（不 fetch、无 usage_type/error），mock 零调用；
	// 单 key 端点返回 200 + 禁用说明（局部失败原则）；PATCH 可落 disabled。
	mock := newMock("mock-dis", "Mock Dis", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	id := addTestKeyType(t, st, "mock-dis", "sk-disabled-key1", store.KeyTypeDisabled, "", "")

	// 1) 聚合：skip=true，fetch 未被调用
	w := doJSON(t, s, http.MethodGet, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp allUsageResponse
	decodeResp(t, w, &resp)
	if len(resp.Results) != 1 || !resp.Results[0].Skip {
		t.Fatalf("disabled key 应标 skip: %+v", resp.Results)
	}
	if resp.Results[0].Usage != nil || resp.Results[0].Error != "" {
		t.Errorf("skip 条目不应有 usage/error: %+v", resp.Results[0])
	}
	if mock.calls.Load() != 0 {
		t.Errorf("disabled key 不应发起 fetch, calls = %d", mock.calls.Load())
	}

	// 2) 单 key 端点：200 + 明确说明
	w2 := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/keys/%d/usage", id), "")
	if w2.Code != http.StatusOK {
		t.Fatalf("single status = %d, want 200", w2.Code)
	}
	var single singleUsageResponse
	decodeResp(t, w2, &single)
	if single.Error == "" || !strings.Contains(single.Error, "禁用") {
		t.Errorf("single error = %q, want 含「禁用」说明", single.Error)
	}
	if single.Usage != nil {
		t.Errorf("disabled key 不应有 usage: %+v", single.Usage)
	}
	if mock.calls.Load() != 0 {
		t.Errorf("single 端点也不应 fetch, calls = %d", mock.calls.Load())
	}

	// 3) PATCH 改回 both → 恢复查询
	patchBody := `{"key_type": "both"}`
	w3 := doJSON(t, s, http.MethodPatch, fmt.Sprintf("/api/keys/%d", id), patchBody)
	if w3.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body=%s", w3.Code, w3.Body.String())
	}
	w4 := doJSON(t, s, http.MethodGet, "/api/usage", "")
	var resp2 allUsageResponse
	decodeResp(t, w4, &resp2)
	if resp2.Results[0].Skip {
		t.Errorf("改回 both 后应恢复查询: %+v", resp2.Results[0])
	}
	if mock.calls.Load() == 0 {
		t.Errorf("恢复后应发起 fetch")
	}
}

func TestSnapshotUsageCarriesAlias(t *testing.T) {
	// Snapshot 不再内联用量（前端逐卡片拉取），此处改为验证新契约：
	// 页面首屏只带 keys/providers，usage 恒为空数组。
	mock := newMock("mock-a", "Mock A", nil, balanceUsage(), nil)
	s, st := newTestServer(t, map[string]provider.Provider{mock.ID(): mock})
	addTestKey(t, st, "mock-a", "sk-abc123456789", "快照别名", "")

	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snap.Usage) != 0 {
		t.Fatalf("usage len = %d, want 0（用量由前端逐卡片拉取）", len(snap.Usage))
	}
	if len(snap.Keys) != 1 || snap.Keys[0].Account != "快照别名" {
		t.Errorf("keys 未透传 account: %+v", snap.Keys)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/keys/reorder
// ---------------------------------------------------------------------------

func TestReorderKeys(t *testing.T) {
	s, st := newTestServer(t, nil)
	id1 := addTestKey(t, st, "p1", "sk-one-1", "", "")
	id2 := addTestKey(t, st, "p2", "sk-two-2", "", "")
	id3 := addTestKey(t, st, "p3", "sk-three-3", "", "")

	// 倒序重排 → 204，ListKeys 反映新顺序
	w := doJSON(t, s, http.MethodPut, "/api/keys/reorder",
		fmt.Sprintf(`{"ids":[%d,%d,%d]}`, id3, id2, id1))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", w.Code, w.Body.String())
	}
	recs, err := st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	wantOrder := []int64{id3, id2, id1}
	for i := range recs {
		if recs[i].ID != wantOrder[i] {
			t.Errorf("重排后顺序 %d: got id %d, want %d", i, recs[i].ID, wantOrder[i])
		}
	}

	// 恢复原序
	w = doJSON(t, s, http.MethodPut, "/api/keys/reorder",
		fmt.Sprintf(`{"ids":[%d,%d,%d]}`, id1, id2, id3))
	if w.Code != http.StatusNoContent {
		t.Fatalf("restore status = %d, want 204", w.Code)
	}
}

func TestReorderKeysValidation(t *testing.T) {
	s, st := newTestServer(t, nil)
	id1 := addTestKey(t, st, "p1", "sk-one-1", "", "")
	id2 := addTestKey(t, st, "p2", "sk-two-2", "", "")

	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", "not json"},
		{"空 ids", `{"ids":[]}`},
		{"遗漏 id", fmt.Sprintf(`{"ids":[%d]}`, id1)},
		{"多余 id", fmt.Sprintf(`{"ids":[%d,%d,999]}`, id1, id2)},
		{"重复 id", fmt.Sprintf(`{"ids":[%d,%d]}`, id1, id1)},
	}
	for _, c := range cases {
		w := doJSON(t, s, http.MethodPut, "/api/keys/reorder", c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// 校验失败不影响原顺序
	recs, err := st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	wantOrder := []int64{id1, id2}
	for i := range recs {
		if recs[i].ID != wantOrder[i] {
			t.Errorf("校验失败后顺序 %d: got id %d, want %d", i, recs[i].ID, wantOrder[i])
		}
	}
}

// TestNormalizeQuotaPercents 守护：配额剩余百分比最多两位小数，
// 浮点格式化长串（60.099999999999994）在用量出口被收敛。
func TestNormalizeQuotaPercents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"60.099999999999994", "60.1"},
		{"89.55078125", "89.55"},
		{"30.5", "30.5"},
		{"100", "100"},
		{"0", "0"},
		{"", ""},
		{"abc", "abc"},
	}
	for _, c := range cases {
		u := &provider.Usage{Quotas: []provider.Quota{{Percent: c.in}}}
		normalizeQuotaPercents(u)
		if u.Quotas[0].Percent != c.want {
			t.Errorf("normalizeQuotaPercents(%q) = %q, want %q", c.in, u.Quotas[0].Percent, c.want)
		}
	}
	normalizeQuotaPercents(nil) // nil 安全
}
