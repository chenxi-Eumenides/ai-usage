package minimax

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
