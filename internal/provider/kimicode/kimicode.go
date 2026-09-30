// Package kimicode 实现 Kimi Code 平台用量查询 adapter。
//
// Kimi Code 的用量端点 https://api.kimi.com/coding/v1/usages 未官方文档化，
// 是社区稳定逆向接口（cc-switch / CodexBar 生产使用）。注意它与
// moonshot（platform.moonshot.cn）是两套独立账号体系：sk-kimi- 前缀的 key
// 在 moonshot 侧会 401，反之亦然，因此本 adapter 独立实现、不共享 balance 端点。
package kimicode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"ai-usage/internal/provider"
)

// usagesURL 是 Kimi Code 用量查询端点（包级变量，便于测试替换）。
var usagesURL = "https://api.kimi.com/coding/v1/usages"

const (
	// requestTimeout 是单个用量查询的超时时间。
	requestTimeout = 10 * time.Second
	// fiveHourWindowDuration 表示 5 小时滚动窗口：TIME_UNIT_MINUTE=300 分钟。
	fiveHourWindowDuration = 300
)

// spec 是 Kimi Code 的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "kimi-code",
	DisplayName: "Kimi Code",
	Aliases:     []string{"kimi-for-coding"},
	KeyPrefixes: []string{"sk-kimi-"},
	ConsoleURL:  "https://www.kimi.com/code",
	UsageType:   provider.UsageTypePlan,
	UsageURLs:   provider.UsageURLs{Plan: "https://www.kimi.com/code/console"},
	PlanMeta: provider.PlanMeta{
		PlanName:  "Kimi Code 订阅套餐",
		PlanPrice: "1024-7168 次/周（best-effort）",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// usagesResponse 是 /coding/v1/usages 的响应结构。
// 数字字段均为 string，原样透传，不做数值转换。
type usagesResponse struct {
	User struct {
		Membership struct {
			Level string `json:"level"`
		} `json:"membership"`
	} `json:"user"`
	Usage  usageWindow  `json:"usage"`
	Limits []usageLimit `json:"limits"`
}

// usageWindow 表示一个配额窗口。
type usageWindow struct {
	Limit     string `json:"limit"`
	Used      string `json:"used"`
	Remaining string `json:"remaining"`
	ResetTime string `json:"resetTime"`
}

// usageLimit 表示 limits 数组中的单个滚动窗口条目。
type usageLimit struct {
	Window struct {
		Duration int    `json:"duration"`
		TimeUnit string `json:"timeUnit"`
	} `json:"window"`
	Detail usageWindow `json:"detail"`
}

// fetchUsage 按类型查询 Kimi Code 用量。仅支持 plan 类型。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypePlan {
		return nil, provider.ErrNotSupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usagesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("kimicode: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
	client := provider.NewHTTPClient(requestTimeout)
	resp, err := client.Do(req)
	if err != nil {
		// 网络错误原样透传（超时、DNS、连接拒绝等）。
		return nil, fmt.Errorf("kimicode: request usages: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("kimicode: %w: HTTP %d", provider.ErrAuth, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("kimicode: %w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimicode: %w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kimicode: %w: response body read failed: %w", provider.ErrParse, err)
	}

	var parsed usagesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// 非 JSON 响应（如网关拦截返回的 HTML 登录页）分类为解析失败。
		return nil, fmt.Errorf("kimicode: %w: JSON decode failed: %w", provider.ErrParse, err)
	}

	return buildUsage(parsed), nil
}

// buildUsage 将响应转换为统一 Usage。
// Quotas 顺序稳定：weekly（usage 字段）在前，5h（limits 滚动窗口）在后。
func buildUsage(parsed usagesResponse) *provider.Usage {
	u := &provider.Usage{
		BalanceType: "quota",
		Quotas:      make([]provider.Quota, 0, 2),
		Plan:        &provider.PlanInfo{Level: parsed.User.Membership.Level},
		UpdatedAt:   time.Now(),
	}

	// usage：周配额。窗口字段全空时视为无数据，跳过。
	if parsed.Usage.Limit != "" || parsed.Usage.Used != "" || parsed.Usage.Remaining != "" {
		u.Quotas = append(u.Quotas, provider.Quota{
			Period:    "weekly",
			Used:      parsed.Usage.Used,
			Limit:     parsed.Usage.Limit,
			Remaining: parsed.Usage.Remaining,
			Percent:   calcPercent(parsed.Usage.Remaining, parsed.Usage.Used, parsed.Usage.Limit),
			ResetAt:   parseResetTime(parsed.Usage.ResetTime),
		})
	}

	// limits：duration==300（分钟）的条目为 5 小时滚动窗口。
	for _, l := range parsed.Limits {
		if l.Window.Duration != fiveHourWindowDuration {
			continue
		}
		u.Quotas = append(u.Quotas, provider.Quota{
			Period:    "5h",
			Used:      l.Detail.Used,
			Limit:     l.Detail.Limit,
			Remaining: l.Detail.Remaining,
			Percent:   calcPercent(l.Detail.Remaining, l.Detail.Used, l.Detail.Limit),
			ResetAt:   parseResetTime(l.Detail.ResetTime),
		})
	}

	return u
}

// calcPercent 计算 remaining/limit 的剩余百分比（remaining/limit*100），语义与
// minimax 的 current_interval_remaining_percent 一致：百分比越大剩余越多，
// 前端进度条展示的就是剩余额度。kimi usages API 不返回 percent 字段，且实测
// 响应中 used 字段缺失（真实响应只有 limit/remaining/resetTime），因此只能按
// remaining/limit 计算。任一侧解析失败或 limit 为 0 时返回空（前端据此不显示
// 进度条）。
//
// 部分场景 API 只返回 used + limit（无 remaining），此时按 limit - used 反算。
func calcPercent(remaining, used, limit string) string {
	if limit == "" {
		return ""
	}
	limitF, err := strconv.ParseFloat(limit, 64)
	if err != nil || limitF == 0 {
		return ""
	}

	// 优先用 remaining；缺失时用 limit - used 反算。
	var remainF float64
	if remaining != "" {
		remainF, err = strconv.ParseFloat(remaining, 64)
		if err != nil {
			return ""
		}
	} else if used != "" {
		usedF, err := strconv.ParseFloat(used, 64)
		if err != nil {
			return ""
		}
		remainF = limitF - usedF
	} else {
		return ""
	}

	return strconv.FormatFloat(remainF/limitF*100, 'f', -1, 64)
}

// parseResetTime 解析 RFC3339 重置时间；失败或为空返回零值（不报错）。
func parseResetTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
