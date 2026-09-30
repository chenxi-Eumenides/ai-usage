// Package server 端到端集成测试。
//
// 本文件测试真实 store + mock provider + 完整 server 装配的联合行为。
// 无真实网络依赖（无外部 HTTP 调用）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-usage/internal/cache"
	"ai-usage/internal/provider"
	"ai-usage/internal/store"
)

// ---------------------------------------------------------------------------
// 集成测试基础设施
// ---------------------------------------------------------------------------

// doInteg 发请求到 Wrap 包裹后的 server handler，模拟完整 HTTP 请求链路。
func doInteg(t *testing.T, handler http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Host = "127.0.0.1:8080"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// integGet 快捷 GET 请求。
func integGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	return doInteg(t, h, http.MethodGet, path, "")
}

// integPost 快捷 POST 请求。
func integPost(t *testing.T, h http.Handler, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	return doInteg(t, h, http.MethodPost, path, body, opts...)
}

// integDecode 从 Recorder 解码 JSON 到 v。
func integDecode(t *testing.T, w *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 全局 registry mock
// ---------------------------------------------------------------------------

var (
	integRegOnce  sync.Once
	integMock1    *mockProvider
	integMock2    *mockProvider
	integMockMimo *mockProvider
)

// integrationMockIDs 保证与真实 adapter 的 id 不冲突，且各测试函数间不重复注册。
func registerIntegrationMocks() {
	integRegOnce.Do(func() {
		integMock1 = newMock("integ-ds", "DeepSeek", []string{"sk-"}, balanceUsage(), nil)
		integMock2 = newMock("integ-mt", "Moonshot", []string{"sk-"}, balanceUsage(), nil)
		integMockMimo = newMock("integ-mimo", "Xiaomi MiMo", nil, nil, provider.ErrNotSupported)
		mustRegMock(integMock1)
		mustRegMock(integMock2)
		mustRegMock(integMockMimo)
	})
}

// mustRegMock 把 mockProvider 的静态元数据转成 Spec 注册（T41 数据驱动注册形态），
// fetch 直接绑定 mock 的 FetchUsage 方法值。
func mustRegMock(m *mockProvider) {
	spec := provider.Spec{
		ID:          m.id,
		DisplayName: m.display,
		Aliases:     m.aliases,
		KeyPrefixes: m.prefixes,
		ConsoleURL:  m.console,
		UsageType:   m.UsageType(),
		UsageURLs:   provider.UsageURLs{Balance: m.consoleUsage, Plan: m.consoleUsage},
	}
	if err := provider.Register(spec, m.FetchUsage); err != nil {
		panic(fmt.Sprintf("register integ mock %q: %v", m.id, err))
	}
}

// globalLookup 查找逻辑：先 id 再 alias，匹配全局 registry。
func globalLookup(id string) (provider.Provider, bool) {
	if p, ok := provider.Get(id); ok {
		return p, true
	}
	return provider.GetByAlias(id)
}

// ---------------------------------------------------------------------------
// 测试 1：全流程（key 管理 → usage）
// ---------------------------------------------------------------------------

func TestIntegrationFullFlow(t *testing.T) {
	registerIntegrationMocks()

	// ---- 步骤 1：创建 store 并直接添加测试 key ----
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for _, key := range []struct{ provider, value string }{
		{"integ-ds", "sk-integ-ds-11111111111111"},
		{"integ-mt", "sk-integ-mt-22222222222222"},
		{"unknown-prov", "uk-unknown-333333333"},
	} {
		if inserted, err := st.AddKey(key.provider, key.value, store.KeyTypeBoth, "", ""); err != nil || !inserted {
			t.Fatalf("AddKey(%s): inserted=%v, err=%v", key.provider, inserted, err)
		}
	}

	// ---- 步骤 2：装配 server（真实 store + cache + 全局 registry Lookup）----
	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	s.Lookup = globalLookup
	handler := Wrap(s.Handler(), "")

	// ---- 步骤 3：GET /api/keys —— 断言 3 个 key，masked 格式，未知 provider 存原名 ----
	w := integGet(t, handler, "/api/keys")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/keys status = %d, body=%s", w.Code, w.Body.String())
	}
	var keys []keyResponse
	integDecode(t, w, &keys)
	if len(keys) != 3 {
		t.Fatalf("keys len = %d, want 3", len(keys))
	}

	byProv := map[string]keyResponse{}
	for _, k := range keys {
		byProv[k.Provider] = k
		// 断言 key 被脱敏
		if strings.Contains(string(w.Body.Bytes()), "sk-integ-ds-111111") ||
			strings.Contains(string(w.Body.Bytes()), "sk-integ-mt-222222") ||
			strings.Contains(string(w.Body.Bytes()), "uk-unknown-333333") {
			t.Errorf("响应体包含明文 key: provider=%s body-fragment", k.Provider)
		}
	}

	// integ-ds：已知 provider → display_name + console_url 非空
	ds := byProv["integ-ds"]
	gotMasked := provider.MaskKey("sk-integ-ds-11111111111111")
	if ds.KeyMasked != gotMasked {
		t.Errorf("integ-ds key_masked = %q, want %q", ds.KeyMasked, gotMasked)
	}
	if ds.ProviderDisplayName != "DeepSeek" {
		t.Errorf("integ-ds display = %q, want DeepSeek", ds.ProviderDisplayName)
	}
	if ds.ConsoleURL == "" {
		t.Errorf("integ-ds console_url 不应为空")
	}

	// integ-mt：已知 provider
	mt := byProv["integ-mt"]
	if mt.ProviderDisplayName != "Moonshot" {
		t.Errorf("integ-mt display = %q, want Moonshot", mt.ProviderDisplayName)
	}

	// unknown-prov：不在 registry → display_name/console_url 空，但 provider 存原名
	unk := byProv["unknown-prov"]
	if unk.ProviderDisplayName != "" {
		t.Errorf("unknown-prov display = %q, want 空", unk.ProviderDisplayName)
	}
	if unk.ConsoleURL != "" {
		t.Errorf("unknown-prov console_url = %q, want 空", unk.ConsoleURL)
	}
	if unk.Provider != "unknown-prov" {
		t.Errorf("unknown-prov provider = %q, want unknown-prov (存原名)", unk.Provider)
	}

	// ---- 步骤 4：GET /api/usage —— 2 个已知 provider 用量正常 + 1 个 error ----
	w = integGet(t, handler, "/api/usage")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/usage status = %d, body=%s", w.Code, w.Body.String())
	}
	var uResp allUsageResponse
	integDecode(t, w, &uResp)
	if len(uResp.Results) != 3 {
		t.Fatalf("usage results len = %d, want 3", len(uResp.Results))
	}

	for _, r := range uResp.Results {
		switch r.Provider {
		case "integ-ds", "integ-mt":
			if r.Error != "" {
				t.Errorf("%s: error = %q, want 空（成功）", r.Provider, r.Error)
			}
			if r.Usage == nil || r.Usage.Balance == nil {
				t.Errorf("%s: usage 不应为空", r.Provider)
			}
		case "unknown-prov":
			if !r.Skip {
				t.Errorf("unknown-prov skip = false, want true（未知 provider 仅存储不查用量）: %+v", r)
			}
			if r.Error != "" || r.Usage != nil {
				t.Errorf("unknown-prov 不应有 error/usage: %+v", r)
			}
		default:
			t.Errorf("意外的 provider: %q", r.Provider)
		}
	}

	// ---- 步骤 5：POST /api/keys 添加新 key → 201 ----
	w = integPost(t, handler, "/api/keys", `{"provider":"integ-ds","key":"sk-newtest-123456","account":"测试新增","note":"集成测试"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/keys status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var added keyResponse
	integDecode(t, w, &added)
	if added.ID == 0 {
		t.Fatalf("新增 key id = 0")
	}
	if added.Provider != "integ-ds" {
		t.Errorf("新增 provider = %q, want integ-ds", added.Provider)
	}
	if added.Account != "测试新增" || added.Note != "集成测试" {
		t.Errorf("新增 account/note 异常: account=%q note=%q", added.Account, added.Note)
	}
	newID := added.ID

	// ---- 步骤 6：PATCH 更新 account → 200 ----
	w = doInteg(t, handler, http.MethodPatch, fmt.Sprintf("/api/keys/%d", newID), `{"account":"集成更新"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var patched keyResponse
	integDecode(t, w, &patched)
	if patched.Account != "集成更新" || patched.Note != "集成测试" {
		t.Errorf("PATCH 后 account=%q note=%q, want account=集成更新 note=集成测试", patched.Account, patched.Note)
	}

	// ---- 步骤 7：DELETE → 204 ----
	w = integDelete(t, handler, fmt.Sprintf("/api/keys/%d", newID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", w.Code)
	}
	// 二次删除 → 404
	w = integDelete(t, handler, fmt.Sprintf("/api/keys/%d", newID))
	if w.Code != http.StatusNotFound {
		t.Errorf("二次 DELETE status = %d, want 404", w.Code)
	}
}

// integDelete 发送 DELETE 请求。
func integDelete(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	return doInteg(t, handler, http.MethodDelete, path, "")
}

// ---------------------------------------------------------------------------
// 测试 2：CSRF —— POST /api/refresh 带恶意 Origin → 403
// ---------------------------------------------------------------------------

func TestIntegrationCSRF(t *testing.T) {
	// 需要真实 store 以避免 nil dereference（handler 内部访问 s.store）
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	handler := Wrap(s.Handler(), "")

	// 恶意 Origin → 403
	w := doInteg(t, handler, http.MethodPost, "/api/refresh", "",
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if w.Code != http.StatusForbidden {
		t.Fatalf("malicious Origin: status = %d, want 403, body=%s", w.Code, w.Body.String())
	}
	var errResp map[string]string
	integDecode(t, w, &errResp)
	if !strings.Contains(errResp["error"], "跨站请求被拒绝") {
		t.Errorf("error = %q, want 含 '跨站请求被拒绝'", errResp["error"])
	}

	// 无 Origin → 放行（curl/CLI）
	w = doInteg(t, handler, http.MethodPost, "/api/refresh", "")
	if w.Code == http.StatusForbidden {
		t.Fatalf("无 Origin 不应 403, body=%s", w.Body.String())
	}

	// GET 请求带恶意 Origin → 放行（读操作不做 CSRF）
	w = doInteg(t, handler, http.MethodGet, "/api/keys", "",
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if w.Code == http.StatusForbidden {
		t.Errorf("GET 带恶意 Origin 不应 403（读操作放行）")
	}
}

// ---------------------------------------------------------------------------
// 测试 3：缓存 —— 连续两次 GET /api/keys/{id}/usage，fetch 只调 1 次
// ---------------------------------------------------------------------------

func TestIntegrationUsageCache(t *testing.T) {
	mock := newMock("cache-test", "CacheTest", nil, balanceUsage(), nil)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	s.Lookup = func(id string) (provider.Provider, bool) {
		if id == mock.ID() {
			return mock, true
		}
		return nil, false
	}
	id := addTestKeyType(t, st, "cache-test", "sk-cache-test-12345678901", store.KeyTypeBalance, "", "")
	handler := Wrap(s.Handler(), "")

	// 第一次拉取
	w := integGet(t, handler, fmt.Sprintf("/api/keys/%d/usage", id))
	if w.Code != http.StatusOK {
		t.Fatalf("first GET usage status = %d, body=%s", w.Code, w.Body.String())
	}
	if mock.calls.Load() != 1 {
		t.Fatalf("首次 fetch calls = %d, want 1", mock.calls.Load())
	}

	// 第二次拉取 → 缓存命中，不再 fetch
	w = integGet(t, handler, fmt.Sprintf("/api/keys/%d/usage", id))
	if w.Code != http.StatusOK {
		t.Fatalf("second GET usage status = %d", w.Code)
	}
	if got := mock.calls.Load(); got != 1 {
		t.Errorf("命中缓存后 fetch calls = %d, want 1（未重新调用）", got)
	}
}

// ---------------------------------------------------------------------------
// 测试 4：刷新 —— POST /api/refresh → cache.Clear 后重新拉取
// ---------------------------------------------------------------------------

func TestIntegrationRefresh(t *testing.T) {
	mock := newMock("ref-test", "RefreshTest", nil, balanceUsage(), nil)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	s.Lookup = func(id string) (provider.Provider, bool) {
		if id == mock.ID() {
			return mock, true
		}
		return nil, false
	}
	addTestKeyType(t, st, "ref-test", "sk-ref-test-12345678901", store.KeyTypeBalance, "", "")
	handler := Wrap(s.Handler(), "")

	// 首次拉取
	integGet(t, handler, "/api/usage")
	if got := mock.calls.Load(); got != 1 {
		t.Fatalf("首次 calls = %d, want 1", got)
	}

	// 再次 GET —— 缓存命中，calls 仍为 1
	integGet(t, handler, "/api/usage")
	if got := mock.calls.Load(); got != 1 {
		t.Errorf("缓存命中后 calls = %d, want 1", got)
	}

	// POST /api/refresh —— 清缓存 + 重新拉取
	w := integPost(t, handler, "/api/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/refresh status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp allUsageResponse
	integDecode(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Errorf("refresh results len = %d, want 1", len(resp.Results))
	}
	if got := mock.calls.Load(); got != 2 {
		t.Errorf("refresh 后 calls = %d, want 2（缓存清空后重新 fetch）", got)
	}
}

// ---------------------------------------------------------------------------
// 测试 5：仅存储 provider（both key 双类型均 ErrNotSupported）→ usage.error="类型不匹配"
// ---------------------------------------------------------------------------

func TestIntegrationErrNotSupported(t *testing.T) {
	registerIntegrationMocks()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	s.Lookup = globalLookup
	addTestKey(t, st, "integ-mimo", "sk-mimo-test-123456789", "MiMo 主key", "mimo note")
	handler := Wrap(s.Handler(), "")

	// 单 key usage
	w := integGet(t, handler, "/api/usage")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/usage status = %d, want 200（局部失败）", w.Code)
	}
	var resp allUsageResponse
	integDecode(t, w, &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Usage == nil || !strings.Contains(r.Usage.Error, "类型不匹配") {
		t.Errorf("ErrNotSupported usage.error = %+v, want 含 类型不匹配", r.Usage)
	}
	if r.KeyID == 0 {
		t.Errorf("key_id 应为非零")
	}
	if r.Provider != "integ-mimo" {
		t.Errorf("provider = %q, want integ-mimo", r.Provider)
	}
	if r.ConsoleURL == "" {
		t.Errorf("console_url 不应为空")
	}
}

// ---------------------------------------------------------------------------
// 测试 6：modelscope forceTTL —— 强制 1h 缓存
// ---------------------------------------------------------------------------

func TestIntegrationModelscopeForceTTL(t *testing.T) {
	ms := newMock("modelscope", "ModelScope 魔搭", nil, balanceUsage(), nil)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// cacheTTL 极短（1ms），但 modelscope 强制 1h 缓存
	s := NewServer(st, cache.New(1*time.Millisecond), 1*time.Millisecond)
	s.Lookup = func(id string) (provider.Provider, bool) {
		if id == ms.ID() {
			return ms, true
		}
		return nil, false
	}
	addTestKeyType(t, st, "modelscope", "ms-integ-test-123456789", store.KeyTypeBalance, "", "")
	handler := Wrap(s.Handler(), "")

	// 第一次拉取
	integGet(t, handler, "/api/usage")
	if got := ms.calls.Load(); got != 1 {
		t.Fatalf("modelscope 首次 calls = %d, want 1", got)
	}

	// 等待默认 TTL(1ms) 过期
	time.Sleep(20 * time.Millisecond)

	// 第二次拉取 → modelscope 1h 缓存仍命中
	integGet(t, handler, "/api/usage")
	if got := ms.calls.Load(); got != 1 {
		t.Errorf("modelscope 第二次 calls = %d, want 1（1h 缓存命中，即使 cacheTTL=1ms 已过期）", got)
	}
}
