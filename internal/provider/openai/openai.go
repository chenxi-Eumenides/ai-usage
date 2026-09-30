// Package openai 实现 OpenAI 平台的用量查询 adapter。
//
// ChatGPT/Codex OAuth access token 经 WHAM 接口查询套餐窗口；普通 API key
// 仅保留 credit grants 作为非官方的余额/授予额度兼容兜底。
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-usage/internal/provider"
)

// 这些包级变量便于测试注入 httptest URL（生产不可变）。
var (
	creditGrantsURL = "https://api.openai.com/v1/dashboard/billing/credit_grants"
	whamUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
)

var httpClient = provider.NewHTTPClient(10 * time.Second)

// spec 是 OpenAI 的静态元数据（T41 数据驱动）。
// 主类型为套餐：OAuth 套餐指向 ChatGPT Codex 用量分析页（T40），API key
// 的 legacy credit grants 余额指向 API 用量页。
var spec = provider.Spec{
	ID:          "openai",
	DisplayName: "OpenAI",
	Aliases:     []string{"openai"},
	KeyPrefixes: []string{"sk-", "eyJ"},
	ConsoleURL:  "https://platform.openai.com",
	ProxyURL:    "socks5://localhost:20170",
	UsageType:   provider.UsageTypePlan,
	UsageURLs: provider.UsageURLs{
		Plan:    "https://chatgpt.com/codex/cloud/settings/analytics",
		Balance: "https://platform.openai.com/usage",
	},
	Credential: &provider.CredentialSpec{
		Kind:        "token",
		Label:       "Token",
		Help:        "在 Codex 或 ChatGPT 等原客户端重新登录刷新授权后，复制新的 OAuth access token（eyJ 开头）粘贴到下方。",
		Placeholder: "粘贴新的 access token（eyJ 开头）",
		Links: []provider.CredentialLink{
			{Label: "打开用量页", URL: "https://platform.openai.com/usage"},
			{Label: "打开套餐分析页", URL: "https://chatgpt.com/codex/cloud/settings/analytics"},
		},
		KeyFallback: true,
	},
	PlanMeta: provider.PlanMeta{
		PlanName: "ChatGPT/Codex 套餐（OAuth）",
		ApiPrice: "OpenAI API 按量（以官方定价为准）",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

type creditGrantsResp struct {
	TotalGranted   json.Number `json:"total_granted"`
	TotalUsed      json.Number `json:"total_used"`
	TotalAvailable json.Number `json:"total_available"`
}

type whamUsageResp struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		PrimaryWindow   *whamWindow `json:"primary_window"`
		SecondaryWindow *whamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type whamWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

// fetchUsage 按凭据类型查询 OpenAI 用量。ChatGPT/Codex OAuth access token 的套餐
// 配额来自 WHAM；普通 API key 保留 credit grants 作为非官方的 best-effort 兼容路径。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypePlan && keyType != provider.UsageTypeBalance {
		return nil, provider.ErrNotSupported
	}
	if isOAuthAccessToken(key) {
		if keyType != provider.UsageTypePlan {
			return nil, provider.ErrNotSupported
		}
		return fetchWHAMUsage(ctx, key)
	}
	return fetchCreditGrants(ctx, key, keyType)
}

func fetchCreditGrants(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, creditGrantsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return expiredTokenUsage(), nil
	}
	if isAuthSignal(body) {
		return expiredTokenUsage(), nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	var grants creditGrantsResp
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&grants); err != nil {
		return nil, fmt.Errorf("%w: %w", provider.ErrParse, err)
	}
	if grants.TotalGranted.String() == "" || grants.TotalUsed.String() == "" || grants.TotalAvailable.String() == "" {
		return nil, fmt.Errorf("%w: openai credit grants response missing totals", provider.ErrParse)
	}

	usage := &provider.Usage{UpdatedAt: time.Now(), Raw: json.RawMessage(body)}
	switch keyType {
	case provider.UsageTypeBalance:
		usage.BalanceType = provider.UsageTypeBalance
		usage.Balance = &provider.Money{Amount: grants.TotalAvailable.String(), Currency: "USD"}
	case provider.UsageTypePlan:
		usage.BalanceType = "quota"
		remaining := grants.TotalAvailable.String()
		usage.Quotas = []provider.Quota{{
			Period:    "granted",
			Used:      grants.TotalUsed.String(),
			Limit:     grants.TotalGranted.String(),
			Remaining: remaining,
			Percent:   remainingPercent(grants.TotalGranted.String(), remaining),
		}}
	}
	return usage, nil
}

func fetchWHAMUsage(ctx context.Context, accessToken string) (*provider.Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whamUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", "codex_cli_rs")
	if accountID := chatGPTAccountID(accessToken); accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return expiredTokenUsage(), nil
	}
	if isAuthSignal(body) {
		return expiredTokenUsage(), nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	var wham whamUsageResp
	if err := json.Unmarshal(body, &wham); err != nil {
		return nil, fmt.Errorf("%w: %w", provider.ErrParse, err)
	}

	usage := &provider.Usage{
		BalanceType: "quota",
		Raw:         json.RawMessage(body),
		UpdatedAt:   time.Now(),
	}
	if wham.PlanType != "" {
		usage.Plan = &provider.PlanInfo{Level: wham.PlanType}
	}
	for _, window := range []struct {
		period string
		window *whamWindow
	}{
		{period: "primary", window: wham.RateLimit.PrimaryWindow},
		{period: "secondary", window: wham.RateLimit.SecondaryWindow},
	} {
		quota, ok := whamQuota(window.period, window.window)
		if ok {
			usage.Quotas = append(usage.Quotas, quota)
		}
	}
	if usage.Plan == nil && len(usage.Quotas) == 0 {
		return &provider.Usage{
			Error:     "OpenAI 套餐用量不可用（响应中无套餐窗口数据），请确认 Token 类型与有效期",
			UpdatedAt: time.Now(),
		}, nil
	}
	return usage, nil
}

func expiredTokenUsage() *provider.Usage {
	return &provider.Usage{ErrorCode: "credential_expired", Error: "OpenAI Token 已失效，请点击「更新 Token」按钮重新粘贴", UpdatedAt: time.Now()}
}

func isAuthSignal(body []byte) bool {
	var payload struct {
		Code     json.RawMessage `json:"code"`
		LoginURL json.RawMessage `json:"loginUrl"`
		Message  string          `json:"message"`
		Data     struct {
			LoginURL json.RawMessage `json:"loginUrl"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	var code string
	if json.Unmarshal(payload.Code, &code) != nil {
		code = string(payload.Code)
	}
	return code == "401" || len(payload.LoginURL) > 0 || len(payload.Data.LoginURL) > 0 || strings.Contains(strings.ToLower(payload.Message), "login")
}

func isOAuthAccessToken(key string) bool {
	parts := strings.Split(key, ".")
	return len(parts) == 3 && strings.HasPrefix(parts[0], "eyJ") && parts[1] != "" && parts[2] != ""
}

func chatGPTAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

func whamQuota(label string, window *whamWindow) (provider.Quota, bool) {
	if window == nil || window.LimitWindowSeconds <= 0 {
		return provider.Quota{}, false
	}

	quota := provider.Quota{Period: whamPeriod(label, window.LimitWindowSeconds)}
	if window.UsedPercent != nil {
		remaining := 100 - *window.UsedPercent
		if remaining < 0 {
			remaining = 0
		} else if remaining > 100 {
			remaining = 100
		}
		quota.Percent = strconv.FormatFloat(remaining, 'f', -1, 64)
	}
	if window.ResetAt > 0 {
		quota.ResetAt = time.Unix(window.ResetAt, 0).UTC()
	}
	return quota, true
}

func whamPeriod(label string, seconds int64) string {
	switch seconds {
	case 5 * 60 * 60:
		return "5h"
	case 7 * 24 * 60 * 60:
		return "weekly"
	}
	if seconds%3600 == 0 {
		return strconv.FormatInt(seconds/3600, 10) + "h"
	}
	return label
}

func remainingPercent(limit, remaining string) string {
	limitValue, err := strconv.ParseFloat(limit, 64)
	if err != nil || limitValue == 0 {
		return ""
	}
	remainingValue, err := strconv.ParseFloat(remaining, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatFloat(remainingValue/limitValue*100, 'f', -1, 64)
}
