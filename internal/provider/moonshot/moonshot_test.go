package moonshot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-usage/internal/provider"
)

const testKey = "sk-test-moonshot"

// mockBalanceServer 启动伪造的余额接口，校验路径与 Authorization 头后返回给定响应。
func mockBalanceServer(t *testing.T, status int, contentType, body string, checkAuth bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me/balance" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if checkAuth && r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Errorf("unexpected Authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useBalanceURL 将 balanceURL 指向 mock 服务器，测试结束自动恢复。
func useBalanceURL(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := balanceURL
	balanceURL = srv.URL + "/v1/users/me/balance"
	t.Cleanup(func() { balanceURL = old })
}

func TestUsageConsoleURL(t *testing.T) {
	if got := spec.UsageURL(provider.UsageTypePlan); got != "" {
		t.Errorf("plan 无用量页，应返回空串, got %q", got)
	}
	want := "https://platform.kimi.com/console/account"
	for _, keyType := range []string{provider.UsageTypeBalance, "both"} {
		if got := spec.UsageURL(keyType); got != want {
			t.Errorf("keyType=%s: got %q, want %q", keyType, got, want)
		}
	}
}

func TestFetchUsage(t *testing.T) {
	srv := mockBalanceServer(t, http.StatusOK, "application/json",
		`{"code":0,"data":{"available_balance":49.58894,"voucher_balance":46.58893,"cash_balance":3.00001}}`, true)
	useBalanceURL(t, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("FetchUsage failed: %v", err)
	}
	if u.BalanceType != "balance" {
		t.Errorf("expected BalanceType=balance, got %q", u.BalanceType)
	}
	if u.Balance == nil {
		t.Fatal("expected non-nil Balance")
	}
	if u.Balance.Amount != "49.58894" {
		t.Errorf("expected Amount=49.58894, got %q", u.Balance.Amount)
	}
	if u.Balance.Currency != "CNY" {
		t.Errorf("expected Currency=CNY, got %q", u.Balance.Currency)
	}
	if u.Error != "" {
		t.Errorf("expected empty Error, got %q", u.Error)
	}
}

func TestQuotaError(t *testing.T) {
	srv := mockBalanceServer(t, http.StatusOK, "application/json",
		`{"code":-1,"scode":"exceeded_current_quota_error","message":"Current usage has reached the quota limit"}`, true)
	useBalanceURL(t, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("expected nil error for code!=0, got %v", err)
	}
	if u == nil {
		t.Fatal("expected non-nil Usage")
	}
	if u.Error == "" {
		t.Fatal("expected non-empty Error for code != 0")
	}
	want := "Moonshot 查询失败：exceeded_current_quota_error: Current usage has reached the quota limit"
	if u.Error != want {
		t.Errorf("Error = %q, want %q", u.Error, want)
	}
}

func TestAuthError(t *testing.T) {
	srv := mockBalanceServer(t, http.StatusUnauthorized, "application/json",
		`{"error":{"message":"Invalid API key","code":"invalid_api_key_error"}}`, false)
	useBalanceURL(t, srv)

	_, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !provider.IsAuthError(err) {
		t.Errorf("expected IsAuthError, got %v", err)
	}
}

func TestParseError(t *testing.T) {
	srv := mockBalanceServer(t, http.StatusOK, "text/plain", `this is not json`, true)
	useBalanceURL(t, srv)

	_, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("expected error on non-JSON response")
	}
	if !provider.IsParseError(err) {
		t.Errorf("expected IsParseError, got %v", err)
	}
}
