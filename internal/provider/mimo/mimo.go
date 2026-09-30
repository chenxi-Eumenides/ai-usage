// Package mimo 实现小米 MiMo 控制台用量查询 adapter。
package mimo

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ai-usage/internal/provider"
)

var (
	balanceURL = "https://platform.xiaomimimo.com/api/v1/balance"
	planURL    = "https://platform.xiaomimimo.com/api/v1/tokenPlan/usage"
	httpClient = provider.NewHTTPClient(10 * time.Second)
)

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

var spec = provider.Spec{
	ID: "mimo", DisplayName: "Xiaomi MiMo", Aliases: []string{"xiaomi"},
	KeyPrefixes: []string{"sk-", "tp-"},
	Credential: &provider.CredentialSpec{
		Kind:        "cookie",
		Label:       "Cookies",
		Help:        "打开下方网页并登录，然后按 F12 打开开发者工具，进入 Network 后刷新页面。找到 /api/v1/balance 或 /api/v1/tokenPlan/usage 请求，复制 Request Headers 中整段 Cookie 并粘贴到下方。",
		Placeholder: "粘贴完整的 Cookie 字符串",
		Links: []provider.CredentialLink{
			{Label: "打开余额页", URL: "https://platform.xiaomimimo.com/console/balance"},
			{Label: "打开套餐管理页", URL: "https://platform.xiaomimimo.com/console/plan-manage"},
		},
	},
	ConsoleURL: "https://mimo.mi.com", UsageType: provider.UsageTypePlan,
	UsageURLs: provider.UsageURLs{
		Plan: "https://platform.xiaomimimo.com/console/plan-manage", Balance: "https://platform.xiaomimimo.com/console/balance",
	},
	UsageNote: "用量经 MiMo 控制台 Cookies 查询，需手动粘贴，Cookies 约 24 小时失效。",
	PlanMeta:  provider.PlanMeta{PlanName: "MiMo Token Plan", PlanPrice: "官方订阅（best-effort）"},
}

func init() { _ = provider.Register(spec, fetchUsage) }

func fetchUsage(ctx context.Context, cookie, keyType string) (*provider.Usage, error) {
	if strings.TrimSpace(cookie) == "" {
		return &provider.Usage{ErrorCode: "credential_missing", Error: "未配置 MiMo Cookies，请点击「更新 Cookies」按钮粘贴", UpdatedAt: time.Now()}, nil
	}
	endpoint := ""
	switch keyType {
	case provider.UsageTypeBalance:
		endpoint = balanceURL
	case provider.UsageTypePlan:
		endpoint = planURL
	default:
		return nil, provider.ErrNotSupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: response body read failed: %w", provider.ErrParse, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return expiredCookies(), nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
		}
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: JSON decode: %w", provider.ErrParse, err)
	}
	code := rawString(envelope["code"])
	if code != "" && code != "0" {
		message := rawString(envelope["message"])
		if message == "" {
			message = rawString(envelope["msg"])
		}
		var dataSignal map[string]json.RawMessage
		_ = json.Unmarshal(envelope["data"], &dataSignal)
		_, topLoginURL := envelope["loginUrl"]
		_, dataLoginURL := dataSignal["loginUrl"]
		if topLoginURL || dataLoginURL || code == "401" || strings.Contains(strings.ToLower(message), "login") || strings.Contains(message, "401") {
			return expiredCookies(), nil
		}
		return &provider.Usage{Error: "MiMo 查询失败：" + cleanBusinessMessage(message), UpdatedAt: time.Now()}, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		return nil, fmt.Errorf("%w: data JSON decode: %w", provider.ErrParse, err)
	}
	usage := &provider.Usage{UpdatedAt: time.Now()}
	switch keyType {
	case provider.UsageTypeBalance:
		usage.BalanceType = provider.UsageTypeBalance
		amount := data["balance"]
		if len(amount) == 0 {
			amount = data["cashBalance"]
		}
		if len(amount) == 0 {
			amount = data["giftBalance"]
		}
		if len(amount) > 0 {
			currency := rawString(data["currency"])
			if currency == "" {
				currency = "CNY"
			}
			usage.Balance = &provider.Money{Amount: rawString(amount), Currency: currency}
		}
	case provider.UsageTypePlan:
		usage.BalanceType = "quota"
		appendItems := func(raw json.RawMessage, defaultPeriod string) {
			var container struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if json.Unmarshal(raw, &container) != nil {
				return
			}
			for _, item := range container.Items {
				name, used, limit := rawString(item["name"]), rawString(item["used"]), rawString(item["limit"])
				usage.Quotas = append(usage.Quotas, provider.Quota{
					Period: remainingPeriod(name, defaultPeriod), Used: used, Limit: limit,
					Remaining: remaining(used, limit), Percent: remainingPercent(used, limit, rawString(item["percent"])),
				})
			}
		}
		appendItems(data["monthUsage"], "monthly")
		appendItems(data["usage"], "套餐额度")
	}
	return usage, nil
}

func expiredCookies() *provider.Usage {
	return &provider.Usage{ErrorCode: "credential_expired", Error: "MiMo Cookies 已失效，请点击「更新 Cookies」按钮重新粘贴", UpdatedAt: time.Now()}
}

func cleanBusinessMessage(message string) string {
	message = html.UnescapeString(htmlTagPattern.ReplaceAllString(message, " "))
	message = strings.Join(strings.Fields(message), " ")
	if message == "" || strings.HasPrefix(message, "{") || strings.HasPrefix(message, "[") {
		return "上游未提供具体原因"
	}
	runes := []rune(message)
	if len(runes) > 120 {
		message = string(runes[:120])
	}
	return message
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func remainingPeriod(name, defaultPeriod string) string {
	if name == "" {
		name = defaultPeriod
	}
	switch name {
	case "month_total_token":
		return "monthly"
	case "plan_total_token":
		return "套餐额度"
	case "compensation_total_token":
		return "补偿额度"
	default:
		return name
	}
}

func remainingPercent(used, limit, rawPercent string) string {
	if percent, err := strconv.ParseFloat(rawPercent, 64); err == nil && !math.IsNaN(percent) && !math.IsInf(percent, 0) {
		if percent >= 0 && percent <= 1 {
			return formatRemainingPercent((1 - percent) * 100)
		}
		if percent > 1 {
			return formatRemainingPercent(100 - percent)
		}
	}

	usedValue, usedErr := strconv.ParseFloat(used, 64)
	limitValue, limitErr := strconv.ParseFloat(limit, 64)
	if usedErr != nil || limitErr != nil || math.IsNaN(usedValue) || math.IsInf(usedValue, 0) ||
		math.IsNaN(limitValue) || math.IsInf(limitValue, 0) || limitValue <= 0 {
		return ""
	}
	return formatRemainingPercent((1 - usedValue/limitValue) * 100)
}

func formatRemainingPercent(percent float64) string {
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	return strconv.FormatFloat(percent, 'f', -1, 64)
}

func remaining(used, limit string) string {
	u, errU := strconv.ParseFloat(used, 64)
	l, errL := strconv.ParseFloat(limit, 64)
	if errU != nil || errL != nil {
		return ""
	}
	return strconv.FormatFloat(l-u, 'f', -1, 64)
}
