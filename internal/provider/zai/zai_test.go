package zai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

const testKey = "91982d3f89d84389a7428f3da81f4621.s9IJLXCUWsyjY6ja"

const (
	balancePath = "/api/biz/account/query-customer-account-report"
	quotaPath   = "/api/monitor/usage/quota/limit"
)

// mockUsageServer 启动伪造的用量接口，校验 path；authSeq 按请求顺序断言
// Authorization 头（"" 表示不校验），并返回对应的 status/body。
func mockUsageServer(t *testing.T, path string, authSeq []string, statusSeq []int, bodySeq []string) *httptest.Server {
	t.Helper()
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if reqCount < len(authSeq) && authSeq[reqCount] != "" &&
			r.Header.Get("Authorization") != authSeq[reqCount] {
			t.Errorf("request %d: unexpected Authorization %q, want %q",
				reqCount, r.Header.Get("Authorization"), authSeq[reqCount])
		}
		status := http.StatusOK
		body := ""
		if reqCount < len(statusSeq) {
			status = statusSeq[reqCount]
		}
		if reqCount < len(bodySeq) {
			body = bodySeq[reqCount]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
		reqCount++
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useUsageURL 将 urlVar 指向 mock 服务器的指定 path，测试结束自动恢复。
func useUsageURL(t *testing.T, urlVar *string, path string, srv *httptest.Server) {
	t.Helper()
	old := *urlVar
	*urlVar = srv.URL + path
	t.Cleanup(func() { *urlVar = old })
}

// okBody 构造实测结构的正常余额响应。
func okBody() string {
	return `{
		"code": 200,
		"success": true,
		"msg": "操作成功",
		"data": {
			"balance": 48.720820000,
			"rechargeAmount": 50.000000,
			"giveAmount": 0.000000,
			"totalSpendAmount": 1.279180000,
			"todaySpendAmount": null,
			"availableBalance": 48.720820000,
			"frozenBalance": 0E-9
		}
	}`
}

// quotaOkBody 构造 GLM Coding Plan 套餐响应：TOKENS_LIMIT unit:3（5h）+
// unit:6（weekly），带绝对积分与已用百分比。
func quotaOkBody() string {
	return `{
		"code": 200,
		"success": true,
		"msg": "操作成功",
		"data": {
			"limits": [
				{
					"type": "TOKENS_LIMIT",
					"unit": 3,
					"number": 5,
					"usage": 12000,
					"currentValue": 3000,
					"remaining": 9000,
					"percentage": 25,
					"nextResetTime": 1756526400000
				},
				{
					"type": "TOKENS_LIMIT",
					"unit": 6,
					"number": 1,
					"usage": 60000,
					"currentValue": 12000,
					"remaining": 48000,
					"percentage": 20,
					"nextResetTime": 1756857600000
				}
			]
		}
	}`
}

// quotaPercentBody 构造纯百分比套餐响应（usage 等绝对积分缺失）：
// 只剩已用百分比，退化百分比制。
func quotaPercentBody() string {
	return `{
		"code": 200,
		"success": true,
		"data": {
			"limits": [
				{
					"type": "TOKENS_LIMIT",
					"unit": 3,
					"percentage": 25
				},
				{
					"type": "TOKENS_LIMIT",
					"unit": 6,
					"percentage": 80
				}
			]
		}
	}`
}

func TestUsageConsoleURL(t *testing.T) {
	wantPlan := "https://www.bigmodel.cn/coding-plan/personal/overview"
	if got := spec.UsageURL(provider.UsageTypePlan); got != wantPlan {
		t.Errorf("plan 用量页: got %q, want %q", got, wantPlan)
	}
	wantBalance := "https://www.bigmodel.cn/finance-center/finance/overview"
	for _, keyType := range []string{provider.UsageTypeBalance, "both"} {
		if got := spec.UsageURL(keyType); got != wantBalance {
			t.Errorf("keyType=%s: got %q, want %q", keyType, got, wantBalance)
		}
	}
}

func TestFetchUsage(t *testing.T) {
	srv := mockUsageServer(t, balancePath, []string{"Bearer " + testKey}, nil, []string{okBody()})
	useUsageURL(t, &balanceURL, balancePath, srv)

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
	if u.Balance.Amount != "48.72082" {
		t.Errorf("expected Balance.Amount=48.72082, got %q", u.Balance.Amount)
	}
	if u.Balance.Currency != "CNY" {
		t.Errorf("expected Balance.Currency=CNY, got %q", u.Balance.Currency)
	}
	if u.Error != "" {
		t.Errorf("expected empty Error, got %q", u.Error)
	}
	if len(u.Quotas) != 1 {
		t.Fatalf("expected 1 consumption quota, got %d", len(u.Quotas))
	}
	q := u.Quotas[0]
	if q.Period != "总消费" || q.Used != "1.27918" || q.Limit != "50" {
		t.Errorf("unexpected consumption quota: %+v", q)
	}
}

func TestFetchPlan(t *testing.T) {
	srv := mockUsageServer(t, quotaPath, []string{"Bearer " + testKey}, nil, []string{quotaOkBody()})
	useUsageURL(t, &quotaURL, quotaPath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage(plan) failed: %v", err)
	}
	if u.BalanceType != "plan" {
		t.Errorf("expected BalanceType=plan, got %q", u.BalanceType)
	}
	if u.Error != "" {
		t.Errorf("expected empty Error, got %q", u.Error)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("expected 2 quotas (5h+weekly), got %d", len(u.Quotas))
	}

	fiveH := u.Quotas[0]
	if fiveH.Period != "5h" {
		t.Errorf("expected first period=5h, got %q", fiveH.Period)
	}
	if fiveH.Used != "3000" || fiveH.Limit != "12000" || fiveH.Remaining != "9000" {
		t.Errorf("unexpected 5h quota: %+v", fiveH)
	}
	if fiveH.Percent != "75" {
		t.Errorf("expected 5h Percent=75 (剩余百分比), got %q", fiveH.Percent)
	}
	if fiveH.ResetAt.IsZero() {
		t.Error("expected non-zero ResetAt for 5h")
	}
	if got := fiveH.ResetAt.UnixMilli(); got != 1756526400000 {
		t.Errorf("expected ResetAt millis 1756526400000, got %d", got)
	}

	weekly := u.Quotas[1]
	if weekly.Period != "weekly" {
		t.Errorf("expected second period=weekly, got %q", weekly.Period)
	}
	if weekly.Used != "12000" || weekly.Limit != "60000" || weekly.Remaining != "48000" {
		t.Errorf("unexpected weekly quota: %+v", weekly)
	}
	if weekly.Percent != "80" {
		t.Errorf("expected weekly Percent=80 (剩余百分比), got %q", weekly.Percent)
	}
}

func TestFetchPlanPercentOnly(t *testing.T) {
	srv := mockUsageServer(t, quotaPath, []string{"Bearer " + testKey}, nil, []string{quotaPercentBody()})
	useUsageURL(t, &quotaURL, quotaPath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage(plan) failed: %v", err)
	}
	if len(u.Quotas) != 2 {
		t.Fatalf("expected 2 quotas, got %d", len(u.Quotas))
	}
	// 纯百分比：percentage=25（已用）→ Percent=75（剩余）。
	fiveH := u.Quotas[0]
	if fiveH.Percent != "75" {
		t.Errorf("expected 5h Percent=75, got %q", fiveH.Percent)
	}
	if fiveH.Used != "" || fiveH.Limit != "" {
		t.Errorf("percent-only 不应填 used/limit: %+v", fiveH)
	}
	weekly := u.Quotas[1]
	if weekly.Percent != "20" {
		t.Errorf("expected weekly Percent=20, got %q", weekly.Percent)
	}
}

func TestPlanIgnoresUnknownLimits(t *testing.T) {
	// TIME_LIMIT（monthly，MCP 工具）与未知 unit 不应进入 Quotas。
	body := `{
		"code": 200, "success": true,
		"data": {
			"limits": [
				{"type":"TOKENS_LIMIT","unit":3,"percentage":10},
				{"type":"TIME_LIMIT","unit":5,"percentage":50},
				{"type":"TOKENS_LIMIT","unit":99,"percentage":1}
			]
		}
	}`
	srv := mockUsageServer(t, quotaPath, []string{"Bearer " + testKey}, nil, []string{body})
	useUsageURL(t, &quotaURL, quotaPath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage(plan) failed: %v", err)
	}
	if len(u.Quotas) != 1 || u.Quotas[0].Period != "5h" {
		t.Errorf("expected only 5h quota, got %+v", u.Quotas)
	}
}

func TestPlanNotSupported(t *testing.T) {
	_, err := fetchUsage(context.Background(), testKey, "bogus")
	if err == nil || !provider.IsNotSupported(err) {
		t.Errorf("expected ErrNotSupported for unknown type, got %v", err)
	}
}

func TestBearerFallback(t *testing.T) {
	// 第一次带 Bearer → 401；第二次裸 key → 成功。
	srv := mockUsageServer(t, balancePath,
		[]string{"Bearer " + testKey, testKey},
		[]int{http.StatusUnauthorized, http.StatusOK},
		[]string{`{"code":401,"msg":"unauthorized"}`, okBody()})
	useUsageURL(t, &balanceURL, balancePath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("FetchUsage failed after fallback: %v", err)
	}
	if u.Error != "" {
		t.Errorf("expected empty Error, got %q", u.Error)
	}
	if u.Balance == nil || u.Balance.Amount != "48.72082" {
		t.Errorf("expected balance after fallback, got %+v", u.Balance)
	}
}

func TestAuthError(t *testing.T) {
	// 两次（Bearer + 裸 key）都 401 → ErrAuth。
	srv := mockUsageServer(t, balancePath,
		[]string{"Bearer " + testKey, testKey},
		[]int{http.StatusUnauthorized, http.StatusUnauthorized},
		[]string{`{"code":401,"msg":"unauthorized"}`, `{"code":401,"msg":"unauthorized"}`})
	useUsageURL(t, &balanceURL, balancePath, srv)

	_, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("expected error when both attempts are 401")
	}
	if !provider.IsAuthError(err) {
		t.Errorf("expected IsAuthError, got %v", err)
	}
}

func TestParseError(t *testing.T) {
	srv := mockUsageServer(t, balancePath, []string{"Bearer " + testKey}, nil, []string{`this is not json`})
	useUsageURL(t, &balanceURL, balancePath, srv)

	_, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err == nil {
		t.Fatal("expected error on non-JSON response")
	}
	if !provider.IsParseError(err) {
		t.Errorf("expected IsParseError, got %v", err)
	}
}

func TestCodeError(t *testing.T) {
	// code != 200（HTTP 仍 200）→ Usage.Error，不返回 error。
	srv := mockUsageServer(t, balancePath, []string{"Bearer " + testKey}, nil,
		[]string{`{"code":500,"msg":"当前用户不存在coding plan","success":false}`})
	useUsageURL(t, &balanceURL, balancePath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("expected nil error for code!=200, got %v", err)
	}
	if u == nil {
		t.Fatal("expected non-nil Usage")
	}
	if u.Error == "" {
		t.Fatal("expected non-empty Error for code != 200")
	}
	if !strings.Contains(u.Error, "当前用户不存在coding plan") {
		t.Errorf("expected message in Error, got %q", u.Error)
	}
	if u.BalanceType != "balance" {
		t.Errorf("expected BalanceType=balance even on error, got %q", u.BalanceType)
	}
}

func TestPlanCodeError(t *testing.T) {
	// 无 coding plan：code=500 → Usage.Error 局部失败，不返回 error。
	srv := mockUsageServer(t, quotaPath, []string{"Bearer " + testKey}, nil,
		[]string{`{"code":500,"msg":"当前用户不存在coding plan","success":false}`})
	useUsageURL(t, &quotaURL, quotaPath, srv)

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("expected nil error for code!=200, got %v", err)
	}
	if u == nil || u.Error == "" {
		t.Fatal("expected non-empty Error for no coding plan")
	}
	if !strings.Contains(u.Error, "当前用户不存在coding plan") {
		t.Errorf("expected message in Error, got %q", u.Error)
	}
	if u.BalanceType != "plan" {
		t.Errorf("expected BalanceType=plan even on error, got %q", u.BalanceType)
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
func withRetryServer(t *testing.T, urlVar *string, path string, failCount int, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	oldURL := *urlVar
	*urlVar = srv.URL + path
	t.Cleanup(func() { *urlVar = oldURL })

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

// TestRetryNetworkErrors 前 2 次连接失败（EOF），第 3 次成功 → FetchUsage 最终成功。
func TestRetryNetworkErrors(t *testing.T) {
	withRetryServer(t, &balanceURL, balancePath, 2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody())
	})

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypeBalance)
	if err != nil {
		t.Fatalf("FetchUsage should succeed after retries, got: %v", err)
	}
	if u == nil || u.Balance == nil || u.Balance.Amount != "48.72082" {
		t.Fatalf("expected balance after retry, got %+v", u)
	}
}

func TestRetryNetworkErrorsPlan(t *testing.T) {
	withRetryServer(t, &quotaURL, quotaPath, 2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, quotaOkBody())
	})

	u, err := fetchUsage(context.Background(), testKey, provider.UsageTypePlan)
	if err != nil {
		t.Fatalf("FetchUsage(plan) should succeed after retries, got: %v", err)
	}
	if u == nil || len(u.Quotas) != 2 {
		t.Fatalf("expected 2 quotas after retry, got %+v", u)
	}
}

func TestMeta(t *testing.T) {
	if spec.ID != "zai" {
		t.Errorf("unexpected ID: %s", spec.ID)
	}
	if spec.DisplayName == "" {
		t.Error("expected non-empty DisplayName")
	}
	if len(spec.Aliases) != 1 || spec.Aliases[0] != "zai" {
		t.Errorf("unexpected Aliases: %v", spec.Aliases)
	}
	if spec.KeyPattern != keyPattern {
		t.Errorf("unexpected KeyPattern: %q, want %q", spec.KeyPattern, keyPattern)
	}
	if len(spec.KeyPrefixes) != 0 {
		t.Errorf("expected no KeyPrefixes, got %v", spec.KeyPrefixes)
	}
	if spec.ConsoleURL != "https://open.bigmodel.cn" {
		t.Errorf("unexpected ConsoleURL: %s", spec.ConsoleURL)
	}
	if spec.UsageType != provider.UsageTypeBalance {
		t.Errorf("unexpected UsageType: %s, want balance", spec.UsageType)
	}
	if m := spec.PlanMeta; m.PlanName != "智谱开放平台按量付费" {
		t.Errorf("unexpected PlanName: %s", m.PlanName)
	}
}
