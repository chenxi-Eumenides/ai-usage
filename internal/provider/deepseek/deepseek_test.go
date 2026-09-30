package deepseek

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-usage/internal/provider"
)

// withBalanceURL 临时替换包级 balanceURL 为测试 server，返回恢复函数。
func withBalanceURL(t *testing.T, url string) {
	t.Helper()
	old := balanceURL
	balanceURL = url
	t.Cleanup(func() { balanceURL = old })
}

func TestRegistered(t *testing.T) {
	p, ok := provider.Get("deepseek")
	if !ok {
		t.Fatal("deepseek not registered via init()")
	}
	if p.ID() != "deepseek" || p.DisplayName() != "DeepSeek" {
		t.Errorf("id/name = %q/%q, want deepseek/DeepSeek", p.ID(), p.DisplayName())
	}
	if len(p.KeyPrefixes()) != 1 || p.KeyPrefixes()[0] != "sk-" {
		t.Errorf("KeyPrefixes = %v, want [sk-]", p.KeyPrefixes())
	}
	if p.ConsoleURL() != "https://platform.deepseek.com" {
		t.Errorf("ConsoleURL = %q", p.ConsoleURL())
	}
}

func TestFetchUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("unexpected path: %s, want /user/balance", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test123" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test123")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"is_available": true,
			"balance_infos": [{
				"currency": "CNY",
				"total_balance": "0.37",
				"granted_balance": "0.00",
				"topped_up_balance": "0.37"
			}]
		}`))
	}))
	defer srv.Close()
	withBalanceURL(t, srv.URL+"/user/balance")

	usage, err := fetchUsage(context.Background(), "sk-test123", provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("FetchUsage error: %v", err)
	}
	if usage.Balance == nil {
		t.Fatal("usage.Balance is nil, want money")
	}
	if usage.Balance.Amount != "0.37" {
		t.Errorf("Amount = %q, want 0.37", usage.Balance.Amount)
	}
	if usage.Balance.Currency != "CNY" {
		t.Errorf("Currency = %q, want CNY", usage.Balance.Currency)
	}
	if usage.BalanceType != "balance" {
		t.Errorf("BalanceType = %q, want balance", usage.BalanceType)
	}
	if usage.Error != "" {
		t.Errorf("Error = %q, want empty", usage.Error)
	}
}

func TestIsAvailableFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"is_available": false,
			"balance_infos": [{
				"currency": "CNY",
				"total_balance": "0.00",
				"granted_balance": "0.00",
				"topped_up_balance": "0.00"
			}]
		}`))
	}))
	defer srv.Close()
	withBalanceURL(t, srv.URL+"/user/balance")

	usage, err := fetchUsage(context.Background(), "sk-test123", provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("FetchUsage error: %v", err)
	}
	if usage.Error == "" {
		t.Error("usage.Error is empty, want 账户不可用")
	}
	if usage.Balance != nil {
		t.Errorf("usage.Balance = %+v, want nil when unavailable", usage.Balance)
	}
}

func TestAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": {"message": "Authentication Fails"}}`))
	}))
	defer srv.Close()
	withBalanceURL(t, srv.URL+"/user/balance")

	_, err := fetchUsage(context.Background(), "sk-bad-key", provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("FetchUsage error is nil, want ErrAuth")
	}
	if !provider.IsAuthError(err) {
		t.Errorf("IsAuthError(%v) = false, want true", err)
	}
}

func TestParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>upstream error</body></html>`))
	}))
	defer srv.Close()
	withBalanceURL(t, srv.URL+"/user/balance")

	_, err := fetchUsage(context.Background(), "sk-test123", provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("FetchUsage error is nil, want ErrParse")
	}
	if !provider.IsParseError(err) {
		t.Errorf("IsParseError(%v) = false, want true", err)
	}
}

func TestNon200WithJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limit exceeded"}`))
	}))
	defer srv.Close()
	withBalanceURL(t, srv.URL+"/user/balance")

	_, err := fetchUsage(context.Background(), "sk-test123", provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("FetchUsage error is nil")
	}
	if !provider.IsRateLimited(err) || !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("error = %q, want rate-limit classification with HTTP 429", err)
	}
}

// TestUnsupportedKeyType 验证 deepseek 仅支持 balance，plan 查询返回 ErrNotSupported。
func TestUsageConsoleURL(t *testing.T) {
	if got := spec.UsageURL(provider.UsageTypePlan); got != "" {
		t.Errorf("plan 无用量页，应返回空串, got %q", got)
	}
	want := "https://platform.deepseek.com/usage"
	for _, keyType := range []string{provider.UsageTypeBalance, "both"} {
		if got := spec.UsageURL(keyType); got != want {
			t.Errorf("keyType=%s: got %q, want %q", keyType, got, want)
		}
	}
}

func TestUnsupportedKeyType(t *testing.T) {
	_, err := fetchUsage(context.Background(), "sk-test123", provider.UsageTypePlan)
	if !provider.IsNotSupported(err) {
		t.Errorf("FetchUsage(plan) should return ErrNotSupported, got %v", err)
	}
}
