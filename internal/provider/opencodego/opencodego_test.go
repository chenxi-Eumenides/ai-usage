package opencodego

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

// withServer 启动 httptest server 并把 usageURL 指向它，测试结束恢复。
func withServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	old := usageURL
	usageURL = srv.URL
	t.Cleanup(func() { usageURL = old })
}

func TestUsageConsoleURL(t *testing.T) {
	want := "https://opencode.ai/workspace/wrk_01KZTF4CR04MGJGW5ETFRPY19G/go"
	if got := spec.UsageURL(provider.UsageTypePlan); got != want {
		t.Errorf("plan: got %q, want %q", got, want)
	}
	if got := spec.UsageURL(provider.UsageTypeBalance); got != "" {
		t.Errorf("balance: 无对应页面，应返回空串, got %q", got)
	}
	if got := spec.UsageURL("both"); got != want {
		t.Errorf("both: got %q, want %q", got, want)
	}
}

func TestFetchUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"usage": {
				"rolling":  { "status": "ok", "percent": 42, "resetsAt": "2026-08-12T16:00:00Z" },
				"weekly":   { "status": "ok", "percent": 30, "resetsAt": "2026-08-18T00:00:00Z" },
				"monthly":  { "status": "ok", "percent": 15, "resetsAt": "2026-09-01T00:00:00Z" }
			}
		}`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u.BalanceType != "quota" {
		t.Errorf("BalanceType = %q, want %q", u.BalanceType, "quota")
	}
	if u.Error != "" {
		t.Errorf("Error = %q, want empty", u.Error)
	}
	if len(u.Quotas) != 3 {
		t.Fatalf("len(Quotas) = %d, want 3", len(u.Quotas))
	}

	if q := u.Quotas[0]; q.Period != "5h" || q.Percent != "58" {
		t.Errorf("Quotas[0] = %+v, want {Period:5h Percent:58}（已用 42%% → 剩余 58%%）", q)
	}
	if u.Quotas[0].ResetAt.IsZero() {
		t.Error("Quotas[0].ResetAt should not be zero")
	}

	if q := u.Quotas[1]; q.Period != "weekly" || q.Percent != "70" {
		t.Errorf("Quotas[1] = %+v, want {Period:weekly Percent:70}（已用 30%% → 剩余 70%%）", q)
	}

	if q := u.Quotas[2]; q.Period != "monthly" || q.Percent != "85" {
		t.Errorf("Quotas[2] = %+v, want {Period:monthly Percent:85}（已用 15%% → 剩余 85%%）", q)
	}
}

func TestNotFoundHTML(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<!DOCTYPE html><html><head><title>OpenCode</title></head><body>Not Found</body></html>`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("404 should return nil error (degradation), got: %v", err)
	}
	if u == nil {
		t.Fatal("expected non-nil Usage with Error")
	}
	if u.Error == "" {
		t.Error("Error should be non-empty for 404")
	}
	if u.Error != "OpenCode Go 用量接口暂不可用（上游返回 404 或非预期响应），请稍后重试" {
		t.Errorf("404 Error = %q", u.Error)
	}
	if provider.IsAuthError(err) {
		t.Error("404 should NOT be ErrAuth (not a key error)")
	}
}

func TestUnauthorized(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error": "unauthorized"}`)
	})

	u, err := fetchUsage(context.Background(), "bad-key", provider.UsageTypePlan)
	if err == nil {
		t.Fatal("401 should return error (dual-meaning)")
	}
	if !provider.IsAuthError(err) {
		t.Errorf("expected ErrAuth, got %v", err)
	}
	if u == nil {
		t.Fatal("401 should also return non-nil Usage with Error")
	}
	if u.Error == "" {
		t.Error("Usage.Error should contain dual-meaning message")
	}
	want := "OpenCode Go 认证失败（HTTP 401），请检查 Key 是否有效"
	if u.Error != want {
		t.Errorf("Error = %q, want %q", u.Error, want)
	}
}

func TestForbidden(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	usage, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if usage == nil || usage.Error != "OpenCode Go 拒绝访问（HTTP 403），请检查套餐或权限条件" {
		t.Fatalf("usage=%+v", usage)
	}
	if !errors.Is(err, provider.ErrUpstream) {
		t.Fatalf("error=%v, want ErrUpstream", err)
	}
}

func TestNonJSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<!DOCTYPE html><html><head><title>OpenCode</title></head><body></body></html>`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("200 HTML should return nil error (degradation), got: %v", err)
	}
	if u == nil {
		t.Fatal("expected non-nil Usage with Error")
	}
	if u.Error == "" {
		t.Error("Error should be non-empty for non-JSON response")
	}
	if u.Error != "OpenCode Go 用量接口暂不可用（上游返回 404 或非预期响应），请稍后重试" {
		t.Errorf("non-JSON Error = %q", u.Error)
	}
	if provider.IsAuthError(err) {
		t.Error("non-JSON should NOT be ErrAuth (not a key error)")
	}
}

// flakyTransport 前 failCount 次 RoundTrip 返回网络错误（EOF），之后转发 inner。
type flakyTransport struct {
	failCount int
	attempts  int
	inner     http.RoundTripper
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.attempts++
	if f.attempts <= f.failCount {
		return nil, io.ErrUnexpectedEOF
	}
	return f.inner.RoundTrip(req)
}

// withRetryServer 启动 httptest server，并在 client 前插入失败 failCount 次的
// 传输层，同时缩短 retryDelays 避免测试等待真实间隔。
func withRetryServer(t *testing.T, failCount int, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	oldURL := usageURL
	usageURL = srv.URL
	t.Cleanup(func() { usageURL = oldURL })

	oldClient := httpClient
	httpClient = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &flakyTransport{failCount: failCount, inner: http.DefaultTransport},
	}
	t.Cleanup(func() { httpClient = oldClient })

	oldDelays := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = oldDelays })
}

// okUsageBody 是正常 usage 响应。
func okUsageBody() string {
	return `{
		"usage": {
			"rolling":  { "status": "ok", "percent": 42, "resetsAt": "2026-08-12T16:00:00Z" },
			"weekly":   { "status": "ok", "percent": 30, "resetsAt": "2026-08-18T00:00:00Z" },
			"monthly":  { "status": "ok", "percent": 15, "resetsAt": "2026-09-01T00:00:00Z" }
		}
	}`
}

// TestRetryNetworkErrors 前 2 次连接失败（EOF），第 3 次成功 → FetchUsage 最终成功。
func TestRetryNetworkErrors(t *testing.T) {
	withRetryServer(t, 2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okUsageBody())
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage should succeed after retries, got: %v", err)
	}
	if u == nil || len(u.Quotas) != 3 {
		t.Fatalf("expected 3 quotas after retry, got %+v", u)
	}
	if u.Error != "" {
		t.Errorf("expected empty Error, got %q", u.Error)
	}
}

// TestRetryExhausted 网络错误永久失败 → 返回带「3 attempts」计数的错误。
func TestRetryExhausted(t *testing.T) {
	withRetryServer(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	_, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err == nil {
		t.Fatal("expected error when all attempts fail")
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("expected '3 attempts' in error, got: %v", err)
	}
	// 确认是网络错误透传，未误分类为认证/解析错误。
	if errors.Is(err, provider.ErrAuth) || errors.Is(err, provider.ErrParse) {
		t.Errorf("network error should not be classified as auth/parse: %v", err)
	}
}

// TestNoRetryOnHTTPError HTTP 404 应直接按降级语义返回，不因重试而延迟。
func TestNoRetryOnHTTPError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><body>Not Found</body></html>`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("404 should degrade with nil error, got: %v", err)
	}
	if u == nil || u.Error == "" {
		t.Fatal("expected non-nil Usage with Error for 404")
	}
}
