package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

func withCreditGrantsURL(t *testing.T, url string) {
	t.Helper()
	old := creditGrantsURL
	creditGrantsURL = url
	t.Cleanup(func() { creditGrantsURL = old })
}

func withWHAMUsageURL(t *testing.T, url string) {
	t.Helper()
	old := whamUsageURL
	whamUsageURL = url
	t.Cleanup(func() { whamUsageURL = old })
}

func oauthAccessToken(t *testing.T, accountID string) string {
	t.Helper()
	claims := map[string]map[string]string{
		"https://api.openai.com/auth": {"chatgpt_account_id": accountID},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal OAuth claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestUsageConsoleURL(t *testing.T) {
	if got := spec.UsageURL(provider.UsageTypeBalance); got != "https://platform.openai.com/usage" {
		t.Errorf("balance: got %q, want usage 页", got)
	}
	for _, keyType := range []string{provider.UsageTypePlan, "both"} {
		if got := spec.UsageURL(keyType); got != "https://chatgpt.com/codex/cloud/settings/analytics" {
			t.Errorf("keyType=%s: got %q, want ChatGPT Codex 用量分析页", keyType, got)
		}
	}
}

func TestRegistered(t *testing.T) {
	p, ok := provider.Get("openai")
	if !ok {
		t.Fatal("openai not registered via init()")
	}
	if p.DisplayName() != "OpenAI" || p.ConsoleURL() != "https://platform.openai.com" {
		t.Fatalf("unexpected provider metadata: %q %q", p.DisplayName(), p.ConsoleURL())
	}
}

func TestSpecRequiresLocalProxy(t *testing.T) {
	if spec.ProxyURL != "socks5://localhost:20170" {
		t.Errorf("ProxyURL = %q, want socks5://localhost:20170", spec.ProxyURL)
	}
	if len(spec.KeyPrefixes) != 2 || spec.KeyPrefixes[0] != "sk-" || spec.KeyPrefixes[1] != "eyJ" {
		t.Errorf("KeyPrefixes = %#v, want sk- and eyJ", spec.KeyPrefixes)
	}
	if spec.Credential == nil || spec.Credential.Kind != "token" || !spec.Credential.KeyFallback {
		t.Fatalf("OpenAI credential spec = %+v, want token with KeyFallback", spec.Credential)
	}
}

func TestFetchPlanUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dashboard/billing/credit_grants" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_granted":20.5,"total_used":4.5,"total_available":16}`))
	}))
	defer srv.Close()
	withCreditGrantsURL(t, srv.URL+"/v1/dashboard/billing/credit_grants")

	usage, err := fetchUsage(context.Background(), "sk-test", provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage error: %v", err)
	}
	if len(usage.Quotas) != 1 || usage.Quotas[0].Limit != "20.5" || usage.Quotas[0].Used != "4.5" || usage.Quotas[0].Remaining != "16" {
		t.Fatalf("unexpected quotas: %+v", usage.Quotas)
	}
	if usage.Quotas[0].Period != "granted" {
		t.Errorf("Period = %q, want granted", usage.Quotas[0].Period)
	}
	if usage.Quotas[0].Percent != "78.04878048780488" {
		t.Errorf("Percent = %q", usage.Quotas[0].Percent)
	}
}

func TestFetchBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_granted":20,"total_used":4,"total_available":16}`))
	}))
	defer srv.Close()
	withCreditGrantsURL(t, srv.URL)

	usage, err := fetchUsage(context.Background(), "sk-test", provider.UsageTypeBalance)
	if err != nil || usage.Balance == nil {
		t.Fatalf("FetchUsage = %+v, %v", usage, err)
	}
	if usage.Balance.Amount != "16" || usage.Balance.Currency != "USD" {
		t.Errorf("balance = %+v", usage.Balance)
	}
}

func TestFetchOAuthPlanUsage(t *testing.T) {
	const accountID = "acct-123"
	token := oauthAccessToken(t, accountID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %q, want WHAM usage path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != accountID {
			t.Errorf("ChatGPT-Account-Id = %q, want %q", got, accountID)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := r.Header.Get("Originator"); got != "codex_cli_rs" {
			t.Errorf("Originator = %q, want codex_cli_rs", got)
		}
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{
				"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1800000000},
				"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":1800600000}
			}
		}`))
	}))
	defer srv.Close()
	withWHAMUsageURL(t, srv.URL+"/backend-api/wham/usage")

	usage, err := fetchUsage(context.Background(), token, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage error: %v", err)
	}
	if usage.BalanceType != "quota" || usage.Plan == nil || usage.Plan.Level != "pro" {
		t.Fatalf("unexpected plan usage: %+v", usage)
	}
	if len(usage.Quotas) != 2 {
		t.Fatalf("quota count = %d, want 2", len(usage.Quotas))
	}
	if quota := usage.Quotas[0]; quota.Period != "5h" || quota.Percent != "58" || !quota.ResetAt.Equal(time.Unix(1800000000, 0).UTC()) {
		t.Errorf("primary quota = %+v, want 5h / 58%% / reset_at", quota)
	}
	if quota := usage.Quotas[1]; quota.Period != "weekly" || quota.Percent != "70" || !quota.ResetAt.Equal(time.Unix(1800600000, 0).UTC()) {
		t.Errorf("secondary quota = %+v, want weekly / 70%% / reset_at", quota)
	}
}

func TestFetchOAuthBalanceIsNotSupported(t *testing.T) {
	_, err := fetchUsage(context.Background(), oauthAccessToken(t, "acct-123"), provider.UsageTypeBalance)
	if !provider.IsNotSupported(err) {
		t.Fatalf("FetchUsage(balance) error = %v, want ErrNotSupported", err)
	}
}

func TestFetchOAuthPlanErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		auth bool
	}{
		{name: "auth", code: http.StatusForbidden, body: `{}`, auth: true},
		{name: "parse", code: http.StatusOK, body: `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			withWHAMUsageURL(t, srv.URL)

			usage, err := fetchUsage(context.Background(), oauthAccessToken(t, "acct-123"), provider.UsageTypePlan)
			if tc.auth {
				if err != nil || usage == nil || usage.ErrorCode != "credential_expired" || usage.Error != "OpenAI Token 已失效，请点击「更新 Token」按钮重新粘贴" {
					t.Fatalf("usage=%+v error=%v, want credential_expired", usage, err)
				}
				return
			}
			if !provider.IsParseError(err) {
				t.Errorf("expected parse error, got usage=%+v err=%v", usage, err)
			}
		})
	}
}

func TestWHAMMissingPlanWindowsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withWHAMUsageURL(t, srv.URL)
	usage, err := fetchUsage(context.Background(), oauthAccessToken(t, "acct-123"), provider.UsageTypePlan)
	want := "OpenAI 套餐用量不可用（响应中无套餐窗口数据），请确认 Token 类型与有效期"
	if err != nil || usage == nil || usage.Error != want {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
}

func TestAuthAndParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		auth bool
	}{
		{name: "auth", code: http.StatusUnauthorized, body: `{}`, auth: true},
		{name: "parse", code: http.StatusOK, body: `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			withCreditGrantsURL(t, srv.URL)
			usage, err := fetchUsage(context.Background(), "sk-test", provider.UsageTypeBalance)
			if tc.auth {
				if err != nil || usage == nil || usage.ErrorCode != "credential_expired" {
					t.Fatalf("usage=%+v error=%v, want credential_expired", usage, err)
				}
				return
			}
			if !provider.IsParseError(err) {
				t.Errorf("expected parse error, got usage=%+v err=%v", usage, err)
			}
		})
	}
}

func TestOAuthBusinessAuthSignalReturnsExpiredCredential(t *testing.T) {
	for _, body := range []string{`{"code":401,"message":"unauthorized"}`, `{"code":1001,"loginUrl":"/login"}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			withWHAMUsageURL(t, srv.URL)
			usage, err := fetchUsage(context.Background(), oauthAccessToken(t, "acct-123"), provider.UsageTypePlan)
			if err != nil || usage == nil || usage.ErrorCode != "credential_expired" {
				t.Fatalf("usage=%+v err=%v, want credential_expired", usage, err)
			}
		})
	}
}

func TestUnsupportedKeyType(t *testing.T) {
	_, err := fetchUsage(context.Background(), "sk-test", "invalid")
	if !provider.IsNotSupported(err) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
