// Package opencodego 实现 OpenCode Go 平台的用量查询 adapter。
//
// 官方端点：GET https://opencode.ai/zen/go/v1/usage（Bearer 认证）。
//
// 降级语义（本 adapter 核心设计）：
// PR #16513 2026-08-11 合入 dev 分支，生产 opencode.ai 是否部署未确认。
// 因此必须区分三种情况：
//   - 200 + JSON → 正常解析 rolling/weekly/monthly 三个配额窗口
//   - 404 / 非 JSON（SPA HTML）→ 返回带 Error 的 Usage，不返回 error（降级，不视为 key 错误）
//   - 401/403 → 返回带 Error 的 Usage 与对应 HTTP 状态错误
//
// 注意：404 时 opencode.ai 返回 SPA HTML 页面而非 JSON，因此必须同时检测
// Content-Type 与响应体特征（前几字节是否 HTML），避免误判为 JSON 解析错误。
package opencodego

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"ai-usage/internal/provider"
)

// usageURL 是 OpenCode Go 用量查询端点。
// 包级变量便于测试注入 httptest URL（生产不可变）。
var usageURL = "https://opencode.ai/zen/go/v1/usage"

// httpClient 复用连接池；Timeout 10s 兜底防挂死。
// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
var httpClient = provider.NewHTTPClient(10 * time.Second)

// retryDelays 是网络层错误自动重试的等待间隔（500ms → 1s，递增）。
// 包级变量便于测试缩短等待；生产共 3 次尝试（1 + 2 次重试）。
var retryDelays = []time.Duration{500 * time.Millisecond, time.Second}

// spec 是 OpenCode Go 的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "opencode-go",
	DisplayName: "OpenCode Go",
	Aliases:     []string{"opencode-go"},
	KeyPrefixes: []string{"sk-"},
	ConsoleURL:  "https://opencode.ai/go",
	UsageType:   provider.UsageTypePlan,
	UsageURLs:   provider.UsageURLs{Plan: "https://opencode.ai/workspace/wrk_01KZTF4CR04MGJGW5ETFRPY19G/go"},
	PlanMeta: provider.PlanMeta{
		PlanName:  "OpenCode Go 订阅",
		PlanPrice: "$5 首月 / $10 月（best-effort）",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// usageResp 对应 GET /zen/go/v1/usage 的响应结构。
type usageResp struct {
	Usage usageWindows `json:"usage"`
}

type usageWindows struct {
	Rolling window `json:"rolling"`
	Weekly  window `json:"weekly"`
	Monthly window `json:"monthly"`
}

type window struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"` // RFC3339 时间字符串
}

// fetchUsage 查询 OpenCode Go 套餐用量。
//
// 降级语义（核心）：
//   - 200 + JSON → 正常解析 rolling/weekly/monthly 三个配额窗口
//   - 404 / 非 JSON（SPA HTML 页面）→ 返回带 Error 的 Usage，err == nil
//     提示上游接口暂不可用，不错误分类为 ErrAuth（避免误报 key 错误）
//   - 401 → 返回认证失败提示；403 → 返回套餐或权限拒绝提示
//
// 网络层错误（EOF/连接重置/超时等，httpClient.Do 返回的 error）自动重试 2 次
// （共 3 次尝试，间隔 500ms → 1s）；HTTP 状态码错误有业务语义，不重试。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypePlan {
		return nil, provider.ErrNotSupported
	}
	resp, err := fetchUsageWithRetry(ctx, key)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return &provider.Usage{
			Error:     "OpenCode Go 认证失败（HTTP 401），请检查 Key 是否有效",
			UpdatedAt: time.Now(),
		}, fmt.Errorf("%w: HTTP 401", provider.ErrAuth)
	}
	if resp.StatusCode == http.StatusForbidden {
		return &provider.Usage{
			Error:     "OpenCode Go 拒绝访问（HTTP 403），请检查套餐或权限条件",
			UpdatedAt: time.Now(),
		}, fmt.Errorf("%w: HTTP 403", provider.ErrUpstream)
	}

	// 404：接口未发布（返回 SPA HTML 页面）
	if resp.StatusCode == http.StatusNotFound {
		return &provider.Usage{
			Error:     "OpenCode Go 用量接口暂不可用（上游返回 404 或非预期响应），请稍后重试",
			UpdatedAt: time.Now(),
		}, nil
	}

	// 非 200 其他状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUpstream, resp.StatusCode)
	}

	// 200 OK：尝试 JSON 解析；非 JSON（SPA HTML）同样降级处理
	var r usageResp
	if err := json.Unmarshal(body, &r); err != nil {
		// 检测是否为 HTML 响应（SPA 降级页面）
		if isHTMLResponse(resp, body) {
			return &provider.Usage{
				Error:     "OpenCode Go 用量接口暂不可用（上游返回 404 或非预期响应），请稍后重试",
				UpdatedAt: time.Now(),
			}, nil
		}
		return nil, fmt.Errorf("%w: %w", provider.ErrParse, err)
	}

	usage := &provider.Usage{
		BalanceType: "quota",
		UpdatedAt:   time.Now(),
		Raw:         json.RawMessage(body),
		Plan:        &provider.PlanInfo{Level: "Go"},
	}

	// 填充三个配额窗口
	usage.Quotas = []provider.Quota{
		buildQuota("rolling", "5h", r.Usage.Rolling),
		buildQuota("weekly", "weekly", r.Usage.Weekly),
		buildQuota("monthly", "monthly", r.Usage.Monthly),
	}

	return usage, nil
}

// fetchUsageWithRetry 发起 GET usage 请求，对传输层错误自动重试。
// 仅重试 httpClient.Do 返回的网络错误（EOF/连接重置/超时）；HTTP 状态码
// 错误（401/403/404/500）有业务语义，交由调用方按降级规则处理。
func fetchUsageWithRetry(ctx context.Context, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

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
	return nil, fmt.Errorf("%w: opencode-go request usages after %d attempts: %w", provider.ErrRetriesExhausted,
		len(retryDelays)+1, lastErr)
}

// buildQuota 根据窗口数据构造 Quota 结构。
// opencode API 的 percent 是「已用」百分比；进度条语义是「剩余」，
// 因此输出 100 - w.Percent（已用 3% → 剩余 97%）。
// Percent 用 FormatFloat('f', -1) 转字符串以去尾零。
// ResetsAt 解析 RFC3339 字符串，解析失败则为零值。
func buildQuota(period, displayPeriod string, w window) provider.Quota {
	q := provider.Quota{
		Period:  displayPeriod,
		Percent: strconv.FormatFloat(100-w.Percent, 'f', -1, 64),
	}
	if w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			q.ResetAt = t
		}
	}
	return q
}

// isHTMLResponse 判断 200 响应体是否为 SPA HTML 页面而非 JSON。
// 同时检查 Content-Type header 和响应体前几字节特征（HTML 标签 <）。
func isHTMLResponse(resp *http.Response, body []byte) bool {
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		// 包含 text/html 即为 HTML
		if bytes.Contains([]byte(ct), []byte("text/html")) {
			return true
		}
		// 明确声明 application/json → 不是 HTML
		if bytes.Contains([]byte(ct), []byte("application/json")) {
			return false
		}
	}
	// 检查响应体前几字节：HTML 通常以 < 开头
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '<' {
		return true
	}
	return false
}
