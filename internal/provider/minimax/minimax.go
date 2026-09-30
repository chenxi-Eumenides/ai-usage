// Package minimax 实现 MiniMax 平台的用量查询 adapter。
//
// 官方端点：GET https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains
// （Coding Plan 套餐，Bearer 认证）。
// 注意：2026-06 起接口字段变更，current_interval_total_count 恒为 0，
// 必须使用 *_remaining_percent 百分数字段计算剩余配额。
package minimax

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

// codingPlanURL 是 MiniMax Coding Plan 剩余量查询端点。
// 包级变量便于测试注入 httptest URL（生产不可变）。
var codingPlanURL = "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains"

// httpClient 复用连接池；Timeout 10s 兜底防挂死。
// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
var httpClient = provider.NewHTTPClient(10 * time.Second)

// spec 是 MiniMax 的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "minimax",
	DisplayName: "MiniMax",
	Aliases:     []string{"minimax-cn-coding-plan"},
	KeyPrefixes: []string{"sk-"},
	ConsoleURL:  "https://platform.minimaxi.com",
	UsageType:   provider.UsageTypePlan,
	UsageURLs:   provider.UsageURLs{Plan: "https://platform.minimaxi.com/console/usage"},
	// MiniMax 无公开定价页，价格为官方订阅（人工核实为准）。
	PlanMeta: provider.PlanMeta{
		PlanName:  "MiniMax Coding Plan",
		PlanPrice: "官方订阅（best-effort）",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// codingPlanResp 对应 GET /coding_plan/remains 的响应结构。
// 仅解析需要的字段；remains_time/weekly_remains_time 为毫秒时间窗，
// 本项目以 percent 为准（total_count 2026-06 后恒为 0，不可用）。
type codingPlanResp struct {
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	ModelRemains []modelRemain `json:"model_remains"`
}

type modelRemain struct {
	ModelName                   string  `json:"model_name"`
	CurrentIntervalRemainingPct float64 `json:"current_interval_remaining_percent"`
	CurrentWeeklyRemainingPct   float64 `json:"current_weekly_remaining_percent"`
	RemainsTime                 int64   `json:"remains_time"`
	WeeklyRemainsTime           int64   `json:"weekly_remains_time"`
}

// fetchUsage 按类型查询 MiniMax Coding Plan 剩余配额。仅支持 plan 类型。
//
// 返回 quota 类型 Usage，Quotas 含 5h + weekly 两个窗口（Percent 字段）。
// base_resp.status_code != 0 → 带 Error 的 Usage（HTTP 200 局部失败），不返回 error；
// 401/403 → ErrAuth；非 JSON → ErrParse；网络错误原样透传。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypePlan {
		return nil, provider.ErrNotSupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codingPlanURL, nil)
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

	var r codingPlanResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: %w", provider.ErrParse, err)
	}

	usage := &provider.Usage{
		BalanceType: "quota",
		UpdatedAt:   time.Now(),
		Raw:         json.RawMessage(body),
	}

	if r.BaseResp.StatusCode != 0 {
		usage.Error = fmt.Sprintf("MiniMax 查询失败（上游业务码 %d），请检查套餐状态", r.BaseResp.StatusCode)
		return usage, nil
	}

	// 过滤 model_name=="video"（video 条目不计入 coding 配额），
	// 取第一个非 video 条目填充 5h + weekly 两个窗口。
	for _, m := range r.ModelRemains {
		if m.ModelName == "video" {
			continue
		}
		usage.Quotas = []provider.Quota{
			{
				Period:  "5h",
				Percent: strconv.FormatFloat(m.CurrentIntervalRemainingPct, 'f', -1, 64),
			},
			{
				Period:  "weekly",
				Percent: strconv.FormatFloat(m.CurrentWeeklyRemainingPct, 'f', -1, 64),
			},
		}
		break
	}
	return usage, nil
}
