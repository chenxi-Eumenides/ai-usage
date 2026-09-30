// Package deepseek 实现 DeepSeek 平台的用量查询 adapter。
//
// 官方余额端点：GET https://api.deepseek.com/user/balance（Bearer 认证）。
package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ai-usage/internal/provider"
)

// balanceURL 是 DeepSeek 余额查询端点。
// 包级变量便于测试注入 httptest URL（生产不可变）。
var balanceURL = "https://api.deepseek.com/user/balance"

// httpClient 复用连接池；Timeout 10s 兜底防挂死。
// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
var httpClient = provider.NewHTTPClient(10 * time.Second)

// spec 是 DeepSeek 的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "deepseek",
	DisplayName: "DeepSeek",
	Aliases:     []string{"deepseek"},
	KeyPrefixes: []string{"sk-"},
	ConsoleURL:  "https://platform.deepseek.com",
	UsageType:   provider.UsageTypeBalance,
	UsageURLs:   provider.UsageURLs{Balance: "https://platform.deepseek.com/usage"},
	// 定价可能过时，实际价格以官方定价页 https://platform.deepseek.com/pricing 为准。
	PlanMeta: provider.PlanMeta{
		PlanName: "DeepSeek API 按量",
		ApiPrice: "官方定价页为准",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// balanceResp 对应 GET /user/balance 的响应结构。
type balanceResp struct {
	IsAvailable  bool          `json:"is_available"`
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

type balanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// fetchUsage 按类型查询 DeepSeek 按量余额。仅支持 balance 类型。
//
// is_available=false 时返回带 Error 的 Usage（HTTP 200 局部失败），不返回 error；
// 其余失败统一走错误分类（ErrAuth/ErrParse），网络错误原样透传。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypeBalance {
		return nil, provider.ErrNotSupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err // 网络错误直接透传
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrAuth, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	var b balanceResp
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("%w: %w", provider.ErrParse, err)
	}
	if len(b.BalanceInfos) == 0 {
		return nil, fmt.Errorf("%w: missing balance_infos", provider.ErrParse)
	}

	usage := &provider.Usage{
		BalanceType: "balance",
		UpdatedAt:   time.Now(),
	}
	if !b.IsAvailable {
		usage.Error = "账户不可用"
		return usage, nil
	}

	bi := b.BalanceInfos[0]
	usage.Balance = &provider.Money{
		Amount:   bi.TotalBalance, // 原样透传 string，不做 float 转换
		Currency: bi.Currency,
	}
	return usage, nil
}
