package minimax

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

// withServer 启动 httptest server 并把 codingPlanURL 指向它，测试结束恢复。
func withServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	old := codingPlanURL
	codingPlanURL = srv.URL
	t.Cleanup(func() { codingPlanURL = old })
}

func TestUsageConsoleURL(t *testing.T) {
	want := "https://platform.minimaxi.com/console/usage"
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
			"base_resp": {"status_code": 0, "status_msg": "success"},
			"model_remains": [{
				"model_name": "general",
				"current_interval_remaining_percent": 72.5,
				"current_weekly_remaining_percent": 85.0,
				"remains_time": 3600000,
				"weekly_remains_time": 86400000
			}]
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
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
	}

	// 5h 窗口：percent 按 float 原样转 string（FormatFloat('f',-1)）
	if q := u.Quotas[0]; q.Period != "5h" || q.Percent != "72.5" {
		t.Errorf("Quotas[0] = %+v, want {Period:5h Percent:72.5}", q)
	}
	// weekly 窗口：85.0 → "85"（去掉尾随零）
	if q := u.Quotas[1]; q.Period != "weekly" || q.Percent != "85" {
		t.Errorf("Quotas[1] = %+v, want {Period:weekly Percent:85}", q)
	}

	// ResetAt 由 remains_time 换算（3600000ms=1h / 86400000ms=1天）。
	// 用调用前后的 now 作为容差区间，避免执行耗时导致 flaky。
	before := time.Now()
	u2, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	after := time.Now()
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	assertResetAtWithin(t, "5h", u2.Quotas[0].ResetAt, before.Add(time.Hour), after.Add(time.Hour))
	assertResetAtWithin(t, "weekly", u2.Quotas[1].ResetAt, before.Add(24*time.Hour), after.Add(24*time.Hour))
}

// assertResetAtWithin 校验 ResetAt 落在 [lo, hi] 内。零值视为失败。
func assertResetAtWithin(t *testing.T, label string, got, lo, hi time.Time) {
	t.Helper()
	if got.IsZero() {
		t.Errorf("%s ResetAt 为零值，want 非零（%v ~ %v）", label, lo, hi)
		return
	}
	if got.Before(lo) || got.After(hi) {
		t.Errorf("%s ResetAt = %v，want 在 [%v, %v] 内", label, got, lo, hi)
	}
}

// TestResetAtAbsentWhenWindowMissing remains_time 为 0 或缺失 → ResetAt 零值且不报错。
func TestResetAtAbsentWhenWindowMissing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"零值", `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[{"model_name":"general","current_interval_remaining_percent":72.5,"current_weekly_remaining_percent":85.0,"remains_time":0,"weekly_remains_time":0}]}`},
		{"字段缺失", `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[{"model_name":"general","current_interval_remaining_percent":72.5,"current_weekly_remaining_percent":85.0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			})
			u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
			if err != nil {
				t.Fatalf("无重置时间不应导致 error, got %v", err)
			}
			if u.Error != "" {
				t.Errorf("Usage.Error = %q, want 空（不降级为失败）", u.Error)
			}
			if len(u.Quotas) != 2 {
				t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
			}
			if !u.Quotas[0].ResetAt.IsZero() {
				t.Errorf("5h ResetAt = %v, want 零值", u.Quotas[0].ResetAt)
			}
			if !u.Quotas[1].ResetAt.IsZero() {
				t.Errorf("weekly ResetAt = %v, want 零值", u.Quotas[1].ResetAt)
			}
		})
	}
}

// TestResetAtFromWindow 单测换算函数本身，避免集成路径掩盖边界。
func TestResetAtFromWindow(t *testing.T) {
	base := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)
	if got := resetAtFromWindow(base, 3600000); !got.Equal(base.Add(time.Hour)) {
		t.Errorf("resetAtFromWindow(3600000) = %v, want %v", got, base.Add(time.Hour))
	}
	if got := resetAtFromWindow(base, 0); !got.IsZero() {
		t.Errorf("resetAtFromWindow(0) = %v, want 零值", got)
	}
	if got := resetAtFromWindow(base, -1); !got.IsZero() {
		t.Errorf("resetAtFromWindow(-1) = %v, want 零值", got)
	}
}

func TestFilterVideo(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"base_resp": {"status_code": 0, "status_msg": "success"},
			"model_remains": [
				{
					"model_name": "video",
					"current_interval_remaining_percent": 10.0,
					"current_weekly_remaining_percent": 20.0,
					"remains_time": 3600000,
					"weekly_remains_time": 86400000
				},
				{
					"model_name": "general",
					"current_interval_remaining_percent": 55.5,
					"current_weekly_remaining_percent": 66.6,
					"remains_time": 3600000,
					"weekly_remains_time": 86400000
				}
			]
		}`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("len(Quotas) = %d, want 2", len(u.Quotas))
	}
	// 断言取到的是 general 条目（55.5/66.6），video 条目被过滤
	if q := u.Quotas[0]; q.Period != "5h" || q.Percent != "55.5" {
		t.Errorf("Quotas[0] = %+v, want 5h/55.5 (video 应被过滤)", q)
	}
	if q := u.Quotas[1]; q.Period != "weekly" || q.Percent != "66.6" {
		t.Errorf("Quotas[1] = %+v, want weekly/66.6 (video 应被过滤)", q)
	}
}

func TestStatusError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"base_resp": {"status_code": 1001, "status_msg": "invalid key"},
			"model_remains": []
		}`)
	})

	u, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u == nil {
		t.Fatal("expected non-nil Usage with Error")
	}
	if u.Error == "" {
		t.Error("Error should be non-empty when status_code != 0")
	}
	if u.Error != "MiniMax 查询失败（上游业务码 1001），请检查套餐状态" {
		t.Errorf("Error = %q", u.Error)
	}
}

func TestAuthError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"base_resp": {"status_code": 401, "status_msg": "unauthorized"}}`)
	})

	_, err := fetchUsage(context.Background(), "bad-key", provider.UsageTypePlan)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !provider.IsAuthError(err) {
		t.Errorf("expected ErrAuth, got %v", err)
	}
}

func TestParseError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `this is not json`)
	})

	_, err := fetchUsage(context.Background(), "test-key", provider.UsageTypePlan)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !provider.IsParseError(err) {
		t.Errorf("expected ErrParse, got %v", err)
	}
}
