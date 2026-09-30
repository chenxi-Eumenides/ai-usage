package kimicode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

// serveUsage 起一个 mock 服务并把 usagesURL 指向它。
func serveUsage(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := usagesURL
	usagesURL = srv.URL
	t.Cleanup(func() { usagesURL = old })
}

// TestUsageConsoleURL 验证 spec 用量页数据：plan/both → 控制台，balance → 空串。
func TestUsageConsoleURL(t *testing.T) {
	want := "https://www.kimi.com/code/console"
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

func TestRegister(t *testing.T) {
	p, ok := provider.Get("kimi-code")
	if !ok {
		t.Fatal("kimi-code not registered via init()")
	}
	if p.ID() != "kimi-code" || p.DisplayName() != "Kimi Code" {
		t.Fatalf("ID/DisplayName = %q/%q", p.ID(), p.DisplayName())
	}
	if len(p.KeyPrefixes()) != 1 || p.KeyPrefixes()[0] != "sk-kimi-" {
		t.Fatalf("KeyPrefixes = %v", p.KeyPrefixes())
	}
	if p.ConsoleURL() != "https://www.kimi.com/code" {
		t.Fatalf("ConsoleURL = %q", p.ConsoleURL())
	}
	m := p.PlanMeta()
	if m.PlanName != "Kimi Code 订阅套餐" || m.PlanPrice != "1024-7168 次/周（best-effort）" || m.ApiPrice != "" {
		t.Fatalf("PlanMeta = %+v", m)
	}
	if byAlias, ok := provider.GetByAlias("kimi-for-coding"); !ok || byAlias.ID() != "kimi-code" {
		t.Fatalf("alias kimi-for-coding -> %v", byAlias)
	}
}

// TestFetchUsage 正常响应 → 断言 Plan.Level 与两个配额窗口（weekly + 5h，顺序稳定）。
func TestFetchUsage(t *testing.T) {
	const body = `{
		"user": { "membership": { "level": "LEVEL_INTERMEDIATE" } },
		"usage": { "limit": "2048", "used": "214", "remaining": "1834", "resetTime": "2026-01-09T15:23:13Z" },
		"limits": [{
			"window": { "duration": 300, "timeUnit": "TIME_UNIT_MINUTE" },
			"detail": { "limit": "200", "used": "139", "remaining": "61", "resetTime": "2026-01-09T12:00:00Z" }
		}],
		"totalQuota": { "limit": "100", "remaining": "99" }
	}`

	serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-kimi-test" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sk-kimi-test")
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want %q", got, "application/json")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	u, err := fetchUsage(context.Background(), "sk-kimi-test", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}

	if u.BalanceType != "quota" {
		t.Errorf("BalanceType = %q, want %q", u.BalanceType, "quota")
	}
	if u.Plan == nil || u.Plan.Level != "LEVEL_INTERMEDIATE" {
		t.Errorf("Plan = %+v, want level LEVEL_INTERMEDIATE", u.Plan)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2 (weekly + 5h)", len(u.Quotas))
	}

	weekly := u.Quotas[0]
	if weekly.Period != "weekly" || weekly.Used != "214" || weekly.Limit != "2048" || weekly.Remaining != "1834" {
		t.Errorf("weekly quota = %+v", weekly)
	}
	if weekly.Percent != "89.55078125" {
		t.Errorf("weekly Percent = %q, want 89.55078125 (1834/2048*100, 剩余百分比)", weekly.Percent)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-01-09T15:23:13Z")
	if !weekly.ResetAt.Equal(wantReset) {
		t.Errorf("weekly ResetAt = %v, want %v", weekly.ResetAt, wantReset)
	}

	fiveHour := u.Quotas[1]
	if fiveHour.Period != "5h" || fiveHour.Used != "139" || fiveHour.Limit != "200" || fiveHour.Remaining != "61" {
		t.Errorf("5h quota = %+v", fiveHour)
	}
	if fiveHour.Percent != "30.5" {
		t.Errorf("5h Percent = %q, want 30.5 (61/200*100, 剩余百分比)", fiveHour.Percent)
	}
	want5hReset, _ := time.Parse(time.RFC3339, "2026-01-09T12:00:00Z")
	if !fiveHour.ResetAt.Equal(want5hReset) {
		t.Errorf("5h ResetAt = %v, want %v", fiveHour.ResetAt, want5hReset)
	}
}

func TestBuildUsagePercentEdgeCases(t *testing.T) {
	cases := []struct {
		name      string
		remaining string
		limit     string
	}{
		{"limit_zero", "10", "0"},
		{"invalid_remaining", "abc", "100"},
		{"invalid_limit", "10", "xyz"},
		{"empty_limit", "10", ""},
		{"empty_remaining", "", "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := buildUsage(usagesResponse{
				Usage: usageWindow{Remaining: tc.remaining, Limit: tc.limit},
			})
			if len(u.Quotas) != 1 {
				t.Fatalf("len(Quotas) = %d, want 1", len(u.Quotas))
			}
			if u.Quotas[0].Percent != "" {
				t.Errorf("Percent = %q, want 空（降级不显示进度条）", u.Quotas[0].Percent)
			}
		})
	}
}

// TestBuildUsageNoUsedField 真实响应场景：kimi API 不返回 used 字段，只有
// limit/remaining。此时 Percent 按 remaining/limit 计算，remaining==limit 时为
// 100%（进度条满宽）。
func TestBuildUsageNoUsedField(t *testing.T) {
	const body = `{
		"user": { "membership": { "level": "LEVEL_TRIAL" } },
		"usage": { "limit": "100", "remaining": "100", "resetTime": "2026-01-09T15:23:13Z" },
		"limits": [{
			"window": { "duration": 300, "timeUnit": "TIME_UNIT_MINUTE" },
			"detail": { "limit": "100", "remaining": "100", "resetTime": "2026-01-09T12:00:00Z" }
		}]
	}`

	serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	u, err := fetchUsage(context.Background(), "sk-kimi-test", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
	}
	// used 字段缺失 → 空串，前端 fmtQuota 不会渲染「/100」。
	for i, q := range u.Quotas {
		if q.Used != "" {
			t.Errorf("Quotas[%d].Used = %q, want 空串（API 不返回 used）", i, q.Used)
		}
		if q.Percent != "100" {
			t.Errorf("Quotas[%d].Percent = %q, want 100 (100/100*100, 剩余百分比)", i, q.Percent)
		}
	}
}

// TestBuildUsageNoRemainingField API 只返回 used + limit（无 remaining）→ 按
// limit - used 反算 percent。典型场景：5h 窗口 used=100, limit=100 → 0%。
func TestBuildUsageNoRemainingField(t *testing.T) {
	const body = `{
		"user": { "membership": { "level": "LEVEL_INTERMEDIATE" } },
		"usage": { "limit": "2048", "used": "214", "remaining": "1834", "resetTime": "2026-01-09T15:23:13Z" },
		"limits": [{
			"window": { "duration": 300, "timeUnit": "TIME_UNIT_MINUTE" },
			"detail": { "limit": "100", "used": "100", "resetTime": "2026-01-09T12:00:00Z" }
		}]
	}`

	serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	u, err := fetchUsage(context.Background(), "sk-kimi-test", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
	}

	weekly := u.Quotas[0]
	if weekly.Percent != "89.55078125" {
		t.Errorf("weekly Percent = %q, want 89.55078125", weekly.Percent)
	}

	fiveHour := u.Quotas[1]
	if fiveHour.Used != "100" || fiveHour.Limit != "100" {
		t.Errorf("5h Used/Limit = %q/%q, want 100/100", fiveHour.Used, fiveHour.Limit)
	}
	if fiveHour.Percent != "0" {
		t.Errorf("5h Percent = %q, want 0 (used up, limit-used=0)", fiveHour.Percent)
	}
}
func TestHTMLResponse(t *testing.T) {
	serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>please login</body></html>"))
	})

	_, err := fetchUsage(context.Background(), "sk-kimi-test", provider.UsageTypePlan)
	if err == nil {
		t.Fatal("FetchUsage() error = nil, want parse error")
	}
	if !provider.IsParseError(err) {
		t.Errorf("IsParseError(%v) = false, want true", err)
	}
}

// TestAuthError 401/403 → IsAuthError 为 true。
func TestAuthError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"invalid key"}`))
			})

			_, err := fetchUsage(context.Background(), "sk-kimi-bad", provider.UsageTypePlan)
			if err == nil {
				t.Fatal("FetchUsage() error = nil, want auth error")
			}
			if !provider.IsAuthError(err) {
				t.Errorf("IsAuthError(%v) = false, want true", err)
			}
		})
	}
}

// TestResetTimeParse resetTime 非法 → ResetAt 零值且不 panic。
func TestResetTimeParse(t *testing.T) {
	const body = `{
		"user": { "membership": { "level": "LEVEL_FREE" } },
		"usage": { "limit": "100", "used": "5", "remaining": "95", "resetTime": "not-a-rfc3339-time" },
		"limits": [{
			"window": { "duration": 300, "timeUnit": "TIME_UNIT_MINUTE" },
			"detail": { "limit": "50", "used": "1", "remaining": "49", "resetTime": "also-invalid" }
		}]
	}`

	serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	u, err := fetchUsage(context.Background(), "sk-kimi-test", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
	}
	if !u.Quotas[0].ResetAt.IsZero() {
		t.Errorf("weekly ResetAt = %v, want zero value", u.Quotas[0].ResetAt)
	}
	if !u.Quotas[1].ResetAt.IsZero() {
		t.Errorf("5h ResetAt = %v, want zero value", u.Quotas[1].ResetAt)
	}
}
