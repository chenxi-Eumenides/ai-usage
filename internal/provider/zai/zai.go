// Package zai 实现智谱开放平台（ZAI / GLM）用量查询 adapter。
//
// 支持两种查询类型：
//   - balance（余额）：端点 /api/biz/account/query-customer-account-report，
//     实测稳定（Bearer 认证），返回可用余额、充值/赠送/总消费等维度。
//   - plan（套餐）：端点 /api/monitor/usage/quota/limit，GLM Coding Plan
//     专属（cc-switch / openchamber 生产使用），返回每 5 小时 + 每周
//     两个时间窗口的限额；无 coding plan 的账户返回
//     {"code":500,"msg":"当前用户不存在coding plan"}，走 Usage.Error 局部失败。
//
// 鉴权注意：实测余额端点 Bearer + key 直接可用；保留先 Bearer 后降级裸 key
// 重试作为防御（cc-switch 实测智谱两种鉴权都收）。
package zai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-usage/internal/provider"
)

// balanceURL 是智谱开放平台按量余额查询端点。
// quotaURL 是 GLM Coding Plan 套餐配额查询端点。
// 包级变量便于测试注入 httptest URL（生产不可变）。
var (
	balanceURL = "https://open.bigmodel.cn/api/biz/account/query-customer-account-report"
	quotaURL   = "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
)

// httpClient 复用连接池；Timeout 10s 兜底防挂死。
// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
var httpClient = provider.NewHTTPClient(10 * time.Second)

// retryDelays 是网络层错误自动重试的等待间隔（500ms → 1s，递增）。
// 包级变量便于测试缩短等待；生产共 3 次尝试（1 + 2 次重试）。
var retryDelays = []time.Duration{500 * time.Millisecond, time.Second}

// keyPattern 是智谱 key 格式：32 位小写 hex + "." + 16 位字母数字。
const keyPattern = `^[0-9a-f]{32}\.[A-Za-z0-9]{16}$`

// spec 是智谱开放平台的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "zai",
	DisplayName: "智谱 ZAI (GLM)",
	Aliases:     []string{"zai"},
	// 智谱 key 统一为 32hex.16alnum 格式（如 91982d3f...f4621.s9IJLXCUWsyjY6ja），
	// 前缀匹配无法表达，用正则完整匹配。
	KeyPattern: keyPattern,
	ConsoleURL: "https://open.bigmodel.cn",
	// 主类型保持 balance：卡片归「余额」组，是否看套餐由用户自行选择 key_type。
	UsageType: provider.UsageTypeBalance,
	UsageURLs: provider.UsageURLs{
		Plan:    "https://www.bigmodel.cn/coding-plan/personal/overview",
		Balance: "https://www.bigmodel.cn/finance-center/finance/overview",
	},
	PlanMeta: provider.PlanMeta{
		PlanName: "智谱开放平台按量付费",
		ApiPrice: "按量计费（余额充值制）",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// balanceResp 对应 GET /api/biz/account/query-customer-account-report 的响应结构。
// 成功：{"code":200,"success":true,"msg":"操作成功",
//
//	"data":{"balance":48.72,"rechargeAmount":50,"giveAmount":0,
//	"totalSpendAmount":1.28,"todaySpendAmount":null,
//	"availableBalance":48.72,"frozenBalance":0,...}}
type balanceResp struct {
	Code    int          `json:"code"`
	Msg     string       `json:"msg"`
	Success bool         `json:"success"`
	Data    *balanceData `json:"data"`
}

type balanceData struct {
	Balance          float64  `json:"balance"`
	RechargeAmount   float64  `json:"rechargeAmount"`
	GiveAmount       float64  `json:"giveAmount"`
	TotalSpendAmount float64  `json:"totalSpendAmount"`
	TodaySpendAmount *float64 `json:"todaySpendAmount"`
	AvailableBalance float64  `json:"availableBalance"`
	FrozenBalance    float64  `json:"frozenBalance"`
}

// quotaResp 对应 GET /api/monitor/usage/quota/limit 的响应结构。
// 成功：{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT",
// "unit":3,"number":5,"usage":...,"currentValue":...,"remaining":...,
// "percentage":12.5,"nextResetTime":...},...]}}
type quotaResp struct {
	Code    int        `json:"code"`
	Msg     string     `json:"msg"`
	Success bool       `json:"success"`
	Data    *quotaData `json:"data"`
}

type quotaData struct {
	Limits []quotaLimit `json:"limits"`
}

// quotaLimit 是 limits 数组中单个配额窗口条目。
// 智谱套餐只有两个时间窗口（用户确认）：TOKENS_LIMIT unit:3 → 5h、unit:6 → weekly。
// percentage 是已用百分比（0-100）；usage/currentValue/remaining 为绝对积分
// （可能为 0 或缺失，此时退化为百分比制）。
type quotaLimit struct {
	Type          string  `json:"type"`
	Unit          int     `json:"unit"`
	Usage         float64 `json:"usage"`
	CurrentValue  float64 `json:"currentValue"`
	Remaining     float64 `json:"remaining"`
	Percentage    float64 `json:"percentage"`
	NextResetTime int64   `json:"nextResetTime"` // epoch 毫秒
}

// fetchUsage 按类型查询智谱用量：balance 查余额端点，plan 查套餐配额端点。
//
// 鉴权降级：先 Bearer <key>，401 时用裸 key（无 Bearer 前缀）重试一次
// （cc-switch 实测智谱两种鉴权都收）；两次都失败 → ErrAuth。
// code != 200 或 success=false → 返回带 Error 的 Usage（HTTP 200 局部失败），
// 不返回 error；响应非 JSON → ErrParse；网络错误自动重试 2 次后透传。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	var url, label string
	switch keyType {
	case provider.UsageTypeBalance:
		url, label = balanceURL, "balance"
	case provider.UsageTypePlan:
		url, label = quotaURL, "plan"
	default:
		return nil, provider.ErrNotSupported
	}

	// 第一次尝试：Bearer 前缀。
	resp, err := zaiRequest(ctx, url, key, true)
	if err != nil {
		return nil, fmt.Errorf("zai: request %s: %w", label, err)
	}

	// 401 → 关闭首次响应体，降级裸 key 重试一次。
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = zaiRequest(ctx, url, key, false)
		if err != nil {
			return nil, fmt.Errorf("zai: request %s (bare key): %w", label, err)
		}
	}
	defer resp.Body.Close()

	// 两次都 401（或 403）：key 无效或过期，分类为认证失败。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("zai: %w: HTTP %d", provider.ErrAuth, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("zai: %w: HTTP %d", provider.ErrRateLimit, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zai: %w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("zai: %w: response body read failed: %w", provider.ErrParse, err)
	}

	switch keyType {
	case provider.UsageTypeBalance:
		return parseBalance(body)
	default:
		return parseQuota(body)
	}
}

// parseBalance 解析余额端点响应为 Usage。
func parseBalance(body []byte) (*provider.Usage, error) {
	var parsed balanceResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		// 非 JSON 响应（如网关拦截返回的 HTML 登录页）分类为解析失败。
		return nil, fmt.Errorf("zai: %w: %w", provider.ErrParse, err)
	}

	// code != 200 或 success=false 或 data 缺失 → 单 provider 局部失败，填入 Usage.Error。
	if parsed.Code != 200 || !parsed.Success || parsed.Data == nil {
		return &provider.Usage{
			BalanceType: provider.UsageTypeBalance,
			Error:       errMsg(parsed.Msg, "余额查询失败"),
			UpdatedAt:   time.Now(),
		}, nil
	}

	return buildBalanceUsage(parsed.Data, body), nil
}

// parseQuota 解析套餐配额端点响应为 Usage。
func parseQuota(body []byte) (*provider.Usage, error) {
	var parsed quotaResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("zai: %w: %w", provider.ErrParse, err)
	}

	// code != 200 或 success=false 或 data 缺失 → 单 provider 局部失败，填入 Usage.Error。
	// 无 coding plan 的账户返回 {"code":500,"msg":"当前用户不存在coding plan"}。
	if parsed.Code != 200 || !parsed.Success || parsed.Data == nil {
		return &provider.Usage{
			BalanceType: provider.UsageTypePlan,
			Error:       errMsg(parsed.Msg, "套餐查询失败"),
			UpdatedAt:   time.Now(),
		}, nil
	}

	return buildPlanUsage(parsed.Data, body), nil
}

// zaiRequest 发起一次用量查询请求，useBearer 控制是否加 Bearer 前缀。
// 网络层错误（EOF/连接重置/超时）自动重试 2 次（共 3 次尝试，间隔递增）。
func zaiRequest(ctx context.Context, url, key string, useBearer bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	auth := key
	if useBearer {
		auth = "Bearer " + key
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")

	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		resp, err := httpClient.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt < len(retryDelays) {
			time.Sleep(retryDelays[attempt])
		}
	}
	return nil, fmt.Errorf("%w: request usage after %d attempts: %w",
		provider.ErrRetriesExhausted, len(retryDelays)+1, lastErr)
}

// buildBalanceUsage 将余额响应转换为统一 Usage。
// Balance 为可用余额（CNY）；另附一个「总消费」Quota 展示 已消费/充值总额。
func buildBalanceUsage(d *balanceData, body []byte) *provider.Usage {
	return &provider.Usage{
		BalanceType: provider.UsageTypeBalance,
		Balance: &provider.Money{
			Amount:   formatFloat(d.AvailableBalance),
			Currency: "CNY",
		},
		Quotas: []provider.Quota{
			{
				Period: "总消费",
				Used:   formatFloat(d.TotalSpendAmount),
				Limit:  formatFloat(d.RechargeAmount),
			},
		},
		UpdatedAt: time.Now(),
		Raw:       json.RawMessage(body),
	}
}

// buildPlanUsage 将套餐配额响应转换为统一 Usage。
// 映射 TOKENS_LIMIT（及 CREDIT_LIMIT）的两个时间窗口：
// unit:3 → "5h"、unit:6 → "weekly"（用户确认智谱套餐只有这两档）。
// percentage 是已用百分比，转剩余百分比（100 - percentage）供前端进度条使用；
// 存在绝对积分（usage>0）时优先用 usage/currentValue/remaining 填充 used/limit/remaining。
func buildPlanUsage(d *quotaData, body []byte) *provider.Usage {
	u := &provider.Usage{
		BalanceType: provider.UsageTypePlan,
		Quotas:      make([]provider.Quota, 0, 2),
		UpdatedAt:   time.Now(),
		Raw:         json.RawMessage(body),
	}
	for _, l := range d.Limits {
		period, ok := quotaPeriod(l)
		if !ok {
			continue
		}
		u.Quotas = append(u.Quotas, buildQuota(l, period))
	}
	return u
}

// quotaPeriod 返回配额窗口对应的 Period；非 TOKENS_LIMIT/CREDIT_LIMIT 或
// 未识别的 unit 返回 false。
func quotaPeriod(l quotaLimit) (string, bool) {
	if l.Type != "TOKENS_LIMIT" && l.Type != "CREDIT_LIMIT" {
		return "", false
	}
	switch l.Unit {
	case 3:
		return "5h", true
	case 6:
		return "weekly", true
	default:
		return "", false
	}
}

// buildQuota 构造单个配额窗口。优先绝对积分（usage>0），否则退化百分比制。
func buildQuota(l quotaLimit, period string) provider.Quota {
	var resetAt time.Time
	if l.NextResetTime > 0 {
		resetAt = time.UnixMilli(l.NextResetTime)
	}

	// 绝对积分：usage=窗口总限额、currentValue=已用、remaining=剩余。
	if l.Usage > 0 {
		return provider.Quota{
			Period:    period,
			Used:      formatFloat(l.CurrentValue),
			Limit:     formatFloat(l.Usage),
			Remaining: formatFloat(l.Remaining),
			Percent:   formatFloat(l.Remaining / l.Usage * 100),
			ResetAt:   resetAt,
		}
	}

	// 纯百分比：percentage=已用百分比，剩余 = 100 - percentage。
	remaining := 100 - l.Percentage
	if remaining < 0 {
		remaining = 0
	}
	return provider.Quota{
		Period:  period,
		Percent: formatFloat(remaining),
		ResetAt: resetAt,
	}
}

// formatFloat 将 JSON number 转最短十进制字符串，避免 float 中间运算精度误差。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// errMsg 从 code != 200 的错误响应构造单 provider 错误信息。
func errMsg(msg, fallback string) string {
	if msg = strings.Join(strings.Fields(strings.TrimSpace(msg)), " "); msg != "" {
		return fmt.Sprintf("智谱%s: %s", fallback, msg)
	}
	return "智谱" + fallback
}
