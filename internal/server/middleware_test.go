package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler 是中间件测试的下游 handler：写入 marker body "ok"。
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
}

// wrapOK 返回 Wrap 包裹 okHandler 后的完整 handler（无认证模式）。
func wrapOK() http.Handler { return Wrap(okHandler(), "") }

// wrapAuth 返回启用 Basic Auth（密码 passwd）后的完整 handler。
func wrapAuth(passwd string) http.Handler { return Wrap(okHandler(), passwd) }

// doWrap 发一个请求到 Wrap 包裹后的 handler。
func doWrap(t *testing.T, method, path, origin, secFetchSite string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = "127.0.0.1:8080"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	w := httptest.NewRecorder()
	wrapOK().ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// OriginCheck
// ---------------------------------------------------------------------------

// GET 请求即使带跨站 Origin 也放行（读操作不做 CSRF 防护）。
func TestOriginCheckReadMethodsPass(t *testing.T) {
	w := doWrap(t, http.MethodGet, "/api/keys", "https://evil.example", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET with cross-site origin: got %d, want 200", w.Code)
	}
}

// 无 Origin 头（curl/CLI）的写请求放行。
func TestOriginCheckNoOriginPasses(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if w := doWrap(t, method, "/api/keys", "", ""); w.Code != http.StatusOK {
			t.Errorf("%s without Origin: got %d, want 200", method, w.Code)
		}
	}
}

// 同源写请求（Origin 的 host[:port] 与 Host 一致，忽略 scheme）放行。
func TestOriginCheckSameOriginPasses(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8080",
	} {
		w := doWrap(t, http.MethodPost, "/api/keys", origin, "")
		if w.Code != http.StatusOK {
			t.Errorf("same-origin %q: got %d, want 200", origin, w.Code)
		}
	}
}

// 跨站写请求 → 403。
func TestOriginCheckCrossOriginBlocked(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		w := doWrap(t, method, "/api/keys", "https://evil.example", "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s cross-origin: got %d, want 403", method, w.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		if body["error"] != "跨站请求被拒绝" {
			t.Errorf("error message: got %q, want %q", body["error"], "跨站请求被拒绝")
		}
	}
}

// Sec-Fetch-Site: cross-site 不再单独拦截（反代场景下 Host 可能被改写），
// 仅依赖 Origin host 匹配做同源校验。
func TestOriginCheckSecFetchSiteCrossSiteAllowed(t *testing.T) {
	w := doWrap(t, http.MethodPost, "/api/keys", "http://127.0.0.1:8080", "cross-site")
	if w.Code != http.StatusOK {
		t.Fatalf("Sec-Fetch-Site: cross-site with matching origin: got %d, want 200", w.Code)
	}
}

// 非法 Origin 格式视为不匹配 → 403。
func TestOriginCheckMalformedOriginBlocked(t *testing.T) {
	w := doWrap(t, http.MethodPost, "/api/keys", "not-a-valid-origin", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("malformed origin: got %d, want 403", w.Code)
	}
}

// localhost 与 127.0.0.1 视为不同 host → 403。
func TestOriginCheckLocalhostVsIPBlocked(t *testing.T) {
	w := doWrap(t, http.MethodPost, "/api/keys", "http://localhost:8080", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("localhost origin vs 127.0.0.1 host: got %d, want 403", w.Code)
	}
}

// originHostMatches 单元测试：scheme 忽略、host 比较、默认端口归一化。
func TestOriginHostMatches(t *testing.T) {
	cases := []struct {
		origin string
		host   string
		want   bool
	}{
		{"http://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"https://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"https://example.com", "example.com", true},
		{"http://localhost:8080", "127.0.0.1:8080", false},
		{"http://example.com:8080", "example.com", false},
		{"http://example.com", "example.com:8080", false},
		{"://bad-origin", "127.0.0.1:8080", false},
		{"", "127.0.0.1:8080", false},
		// 反代场景：Origin 带端口、Host 无端口（Nginx 剥掉端口）
		{"http://example.com:81", "example.com", false},
		{"http://example.com:81", "example.com:81", true},
		// 默认端口归一化：http:80 ≈ 无端口，https:443 ≈ 无端口
		{"http://example.com:80", "example.com", true},
		{"https://example.com:443", "example.com", true},
		{"http://example.com", "example.com:80", true},
	}
	for _, c := range cases {
		if got := originHostMatches(c.origin, c.host); got != c.want {
			t.Errorf("originHostMatches(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

// 下游 panic → 500 中文错误信息，进程不崩。
func TestRecoverMiddleware(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Wrap(panicHandler, "")

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("panic: got %d, want 500", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	want := "服务内部发生错误，请稍后重试；若持续出现请查看服务日志"
	if body["error"] != want {
		t.Errorf("error message: got %q, want %q", body["error"], want)
	}
}

// Recover 后进程可继续服务后续请求。
func TestRecoverMiddlewareProcessSurvives(t *testing.T) {
	flaky := 0
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flaky++
		if flaky == 1 {
			panic("boom")
		}
		w.WriteHeader(http.StatusNoContent)
	}), "")

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	if flaky != 2 {
		t.Fatalf("second request not served: flaky=%d, want 2", flaky)
	}
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// 日志格式为 "method path status duration"，且不含查询参数与敏感头。
func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	req := httptest.NewRequest(http.MethodPost, "/api/keys?secret=leak", nil)
	req.Header.Set("Authorization", "Bearer sk-leak-me")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	wrapOK().ServeHTTP(w, req)

	line := buf.String()
	if !strings.Contains(line, "POST /api/keys 200") {
		t.Errorf("log line missing method/path/status: %q", line)
	}
	if strings.Contains(line, "sk-leak-me") || strings.Contains(line, "secret=leak") {
		t.Errorf("log line must not contain sensitive data (query/auth): %q", line)
	}
}

// ---------------------------------------------------------------------------
// Auth（Basic Auth，可选）
// ---------------------------------------------------------------------------

// doAuth 发一个请求到启用密码认证的 handler，可携带 Basic 凭证。
func doAuth(t *testing.T, passwd, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	if user != "" || pw != "" {
		req.SetBasicAuth(user, pw)
	}
	w := httptest.NewRecorder()
	wrapAuth(passwd).ServeHTTP(w, req)
	return w
}

// 未设置密码（passwd=""）时直接放行，保持无认证模式向后兼容。
func TestAuthDisabledPasses(t *testing.T) {
	w := doAuth(t, "", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("no passwd: got %d, want 200", w.Code)
	}
}

// 启用认证但请求无凭证 → 401 + WWW-Authenticate 挑战头。
func TestAuthMissingCredentials401(t *testing.T) {
	w := doAuth(t, "secret123", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: got %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="ai-usage"` {
		t.Errorf("WWW-Authenticate = %q, want Basic realm=\"ai-usage\"", got)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["error"] != "未授权" {
		t.Errorf("error message: got %q, want %q", body["error"], "未授权")
	}
}

// 密码错误 → 401。
func TestAuthWrongPassword401(t *testing.T) {
	w := doAuth(t, "secret123", "admin", "wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", w.Code)
	}
}

// 用户名不是 admin → 401（即使密码正确）。
func TestAuthWrongUsername401(t *testing.T) {
	w := doAuth(t, "secret123", "bob", "secret123")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong username: got %d, want 401", w.Code)
	}
}

// 正确凭证（admin + 正确密码）→ 200 放行。
func TestAuthValidCredentials200(t *testing.T) {
	w := doAuth(t, "secret123", "admin", "secret123")
	if w.Code != http.StatusOK {
		t.Fatalf("valid credentials: got %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", w.Body.String(), "ok")
	}
}

// 认证对所有方法生效（GET 也拦截），与 OriginCheck 只拦写方法不同。
func TestAuthEnforcedOnGet(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/keys", nil)
		w := httptest.NewRecorder()
		wrapAuth("secret123").ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: got %d, want 401", method, w.Code)
		}
	}
}
