package mimo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ai-usage/internal/provider"
)

func withTestURLs(t *testing.T, balance, plan string) {
	t.Helper()
	oldBalance, oldPlan := balanceURL, planURL
	balanceURL, planURL = balance, plan
	t.Cleanup(func() { balanceURL, planURL = oldBalance, oldPlan })
}

func TestBalanceUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "session=secret; token=abc" {
			t.Errorf("Cookie = %q", got)
		}
		w.Write([]byte(`{"code":0,"data":{"balance":12.5,"cashBalance":"10.5","giftBalance":"2","currency":"CNY"}}`))
	}))
	defer srv.Close()
	withTestURLs(t, srv.URL+"/balance", srv.URL+"/plan")
	u, err := fetchUsage(context.Background(), "session=secret; token=abc", provider.UsageTypeBalance)
	if err != nil {
		t.Fatal(err)
	}
	if u.Balance == nil || u.Balance.Amount != "12.5" || u.Balance.Currency != "CNY" {
		t.Fatalf("balance = %+v", u.Balance)
	}
}

func TestPlanUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plan" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"code":0,"data":{"monthUsage":{"items":[{"name":"month_total_token","used":16.61,"limit":100,"percent":0.1661},{"name":"compensation_total_token","used":5,"limit":10,"percent":0.5}]},"usage":{"items":[{"name":"plan_total_token","used":5,"limit":50,"percent":"10"}]}}}`))
	}))
	defer srv.Close()
	withTestURLs(t, srv.URL+"/balance", srv.URL+"/plan")
	u, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypePlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Quotas) != 3 {
		t.Fatalf("quotas = %+v", u.Quotas)
	}
	if q := u.Quotas[0]; q.Period != "monthly" || q.Percent != "83.39" {
		t.Errorf("month quota = %+v, want monthly with remaining percent 83.39", q)
	}
	if q := u.Quotas[1]; q.Period != "补偿额度" || q.Percent != "50" {
		t.Errorf("compensation quota = %+v, want remaining percent 50", q)
	}
	if q := u.Quotas[2]; q.Period != "套餐额度" || q.Used != "5" || q.Percent != "90" {
		t.Errorf("plan quota = %+v, want 套餐额度 with remaining percent 90", q)
	}
}

func TestPlanUsageEmptyItemNamesUseContainerDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"monthUsage":{"items":[{"used":25,"limit":100}]},"usage":{"items":[{"used":5,"limit":50}]}}}`))
	}))
	defer srv.Close()
	withTestURLs(t, srv.URL, srv.URL)
	u, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypePlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Quotas) != 2 || u.Quotas[0].Period != "monthly" || u.Quotas[1].Period != "套餐额度" {
		t.Fatalf("empty-name periods = %+v, want monthly and 套餐额度", u.Quotas)
	}
	if u.Quotas[0].Percent != "75" || u.Quotas[1].Percent != "90" {
		t.Errorf("fallback percentages = %q, %q, want 75, 90", u.Quotas[0].Percent, u.Quotas[1].Percent)
	}
}

func TestRemainingPercent(t *testing.T) {
	cases := []struct {
		name, used, limit, rawPercent, want string
	}{
		{"fraction is used ratio", "16.61", "100", "0.1661", "83.39"},
		{"percent above one is used percent", "16.61", "100", "16.61", "83.39"},
		{"fallback from used and limit", "25", "100", "", "75"},
		{"invalid raw falls back", "5", "50", "bad", "90"},
		{"clamp below zero", "", "", "120", "0"},
		{"clamp above one hundred", "-5", "100", "", "100"},
		{"missing values", "", "100", "", ""},
		{"non-positive limit", "5", "0", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remainingPercent(tc.used, tc.limit, tc.rawPercent); got != tc.want {
				t.Errorf("remainingPercent(%q, %q, %q) = %q, want %q", tc.used, tc.limit, tc.rawPercent, got, tc.want)
			}
		})
	}
}

func TestCookiesExpired(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`},
		{"login", http.StatusOK, `{"code":1001,"message":"login required","loginUrl":"/login"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); w.Write([]byte(test.body)) }))
			defer srv.Close()
			withTestURLs(t, srv.URL, srv.URL)
			u, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypeBalance)
			if err != nil || u.ErrorCode != "credential_expired" {
				t.Fatalf("usage=%+v err=%v", u, err)
			}
		})
	}
}

func TestMissingCookies(t *testing.T) {
	u, err := fetchUsage(context.Background(), " ", provider.UsageTypeBalance)
	if err != nil || u.ErrorCode != "credential_missing" || u.Error != "未配置 MiMo Cookies，请点击「更新 Cookies」按钮粘贴" {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
}

func TestInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }))
	defer srv.Close()
	withTestURLs(t, srv.URL, srv.URL)
	_, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypePlan)
	if !provider.IsParseError(err) {
		t.Fatalf("error = %v, want ErrParse", err)
	}
}

// TestMetadata 验证 spec 元数据正确 + init() 自注册链路。
func TestMetadata(t *testing.T) {
	if got := spec.ID; got != "mimo" {
		t.Errorf("expected ID=mimo, got %q", got)
	}
	if got := spec.DisplayName; got != "Xiaomi MiMo" {
		t.Errorf("expected DisplayName=Xiaomi MiMo, got %q", got)
	}
	if !reflect.DeepEqual(spec.Aliases, []string{"xiaomi"}) {
		t.Errorf("expected Aliases=[xiaomi], got %v", spec.Aliases)
	}
	if !reflect.DeepEqual(spec.KeyPrefixes, []string{"sk-", "tp-"}) {
		t.Errorf("expected KeyPrefixes=[sk- tp-], got %v", spec.KeyPrefixes)
	}
	// Token Plan 套餐 key 前缀被覆盖。
	if !strings.HasPrefix("tp-syntheticexamplekey000000000000000000000000", spec.KeyPrefixes[1]) {
		t.Errorf("expected tp- key covered by KeyPrefixes[1]=%q", spec.KeyPrefixes[1])
	}
	if got := spec.ConsoleURL; got != "https://mimo.mi.com" {
		t.Errorf("expected ConsoleURL=https://mimo.mi.com, got %q", got)
	}
	if got := spec.PlanMeta.PlanName; got != "MiMo Token Plan" {
		t.Errorf("expected PlanName=MiMo Token Plan, got %q", got)
	}
	if spec.UsageNote != "用量经 MiMo 控制台 Cookies 查询，需手动粘贴，Cookies 约 24 小时失效。" {
		t.Errorf("unexpected UsageNote: %q", spec.UsageNote)
	}

	// init() 自注册链路：注册表可查且 note 透传
	p, ok := provider.Get("mimo")
	if !ok {
		t.Fatal("mimo not registered via init()")
	}
	if p.UsageNote() != spec.UsageNote {
		t.Errorf("UsageNote mismatch: got %q", p.UsageNote())
	}
}

func TestRegisteredCredentialSpec(t *testing.T) {
	p, ok := provider.Get("mimo")
	if !ok {
		t.Fatal("mimo not registered")
	}
	credential, ok := p.(interface {
		CredentialSpec() *provider.CredentialSpec
	})
	if !ok || credential.CredentialSpec() == nil || credential.CredentialSpec().Kind != "cookie" {
		t.Fatalf("credential spec missing or invalid")
	}
}

func TestBusinessErrorParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":3,"message":"denied"}`)) }))
	defer srv.Close()
	withTestURLs(t, srv.URL, srv.URL)
	u, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypeBalance)
	if err != nil || u == nil || u.Error != "MiMo 查询失败：denied" {
		t.Fatalf("usage=%+v error=%v", u, err)
	}
}

func TestBusinessErrorSanitizesAndTruncatesHTML(t *testing.T) {
	message := "<b>" + strings.Repeat("长", 130) + "</b>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":3,"message":%q}`, message)
	}))
	defer srv.Close()
	withTestURLs(t, srv.URL, srv.URL)
	u, err := fetchUsage(context.Background(), "sid=x", provider.UsageTypeBalance)
	if err != nil || u == nil {
		t.Fatalf("usage=%+v error=%v", u, err)
	}
	clean := strings.TrimPrefix(u.Error, "MiMo 查询失败：")
	if strings.Contains(u.Error, "<b>") || len([]rune(clean)) != 120 {
		t.Fatalf("usage=%+v error=%v", u, err)
	}
}

func TestUsageConsoleURL(t *testing.T) {
	if got := spec.UsageURL(provider.UsageTypeBalance); got != "https://platform.xiaomimimo.com/console/balance" {
		t.Errorf("balance: got %q, want 余额页", got)
	}
	for _, keyType := range []string{provider.UsageTypePlan, "both"} {
		if got := spec.UsageURL(keyType); got != "https://platform.xiaomimimo.com/console/plan-manage" {
			t.Errorf("keyType=%s: got %q, want 套餐管理页", keyType, got)
		}
	}
}
