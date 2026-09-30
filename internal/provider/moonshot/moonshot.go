// Package moonshot 实现 Moonshot（Kimi 开放平台）用量查询 adapter。
//
// 国内站官方余额端点：GET https://api.moonshot.cn/v1/users/me/balance（Bearer 认证）。
// 注意：国际站 api.moonshot.ai 是独立体系，本 adapter 面向国内站（用户 key 为 moonshotai-cn）。
package moonshot

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

// balanceURL 是 Moonshot 余额查询端点。
// 包级变量便于测试注入 httptest URL（生产不可变）。
var balanceURL = "https://api.moonshot.cn/v1/users/me/balance"

// httpClient 复用连接池；Timeout 10s 兜底防挂死。
// 禁用代理直连官方 API（避免继承 HTTPS_PROXY 导致 opencode.ai 等域名 EOF）。
var httpClient = provider.NewHTTPClient(10 * time.Second)

// spec 是 Moonshot（Kimi 开放平台）的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "moonshot-cn",
	DisplayName: "Moonshot (Kimi 开放平台)",
	Aliases:     []string{"moonshotai-cn"},
	KeyPrefixes: []string{"sk-"},
	ConsoleURL:  "https://platform.moonshot.cn",
	UsageType:   provider.UsageTypeBalance,
	UsageURLs:   provider.UsageURLs{Balance: "https://platform.kimi.com/console/account"},
	PlanMeta: provider.PlanMeta{
		PlanName: "Moonshot API 按量",
		ApiPrice: "官方定价页为准",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// balanceResp 对应 GET /v1/users/me/balance 的响应结构。
// 成功：{"code":0,"data":{"available_balance":49.58894,"voucher_balance":46.58893,"cash_balance":3.00001}}
// 失败：{"code":<非0>,"scode":"<错误码>","message":"<描述>"}（HTTP 通常仍为 200）
type balanceResp struct {
	Code    int          `json:"code"`
	Scode   string       `json:"scode"`
	Message string       `json:"message"`
	Data    *balanceData `json:"data"`
}

type balanceData struct {
	AvailableBalance float64 `json:"available_balance"` // 可用余额（现金 + 赠送券）
	VoucherBalance   float64 `json:"voucher_balance"`   // 赠送券余额
	CashBalance      float64 `json:"cash_balance"`      // 现金余额
}

// fetchUsage 按类型查询 Moonshot 按量余额。仅支持 balance 类型。
//
// code != 0 时返回带 Error 的 Usage（HTTP 200 局部失败），不返回 error；
// 401/403 → ErrAuth，响应非 JSON → ErrParse，网络错误原样透传。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if keyType != provider.UsageTypeBalance {
		return nil, provider.ErrNotSupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

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
		return nil, fmt.Errorf("%w: JSON decode failed: %w", provider.ErrParse, err)
	}

	// code != 0 → 单 provider 局部失败，填入 Usage.Error 而非返回 error
	if b.Code != 0 {
		return &provider.Usage{
			BalanceType: "balance",
			Error:       "Moonshot 查询失败：" + balanceErrMsg(b),
			UpdatedAt:   time.Now(),
		}, nil
	}

	if b.Data == nil {
		return nil, fmt.Errorf("%w: missing balance data", provider.ErrParse)
	}

	return &provider.Usage{
		BalanceType: "balance",
		Balance: &provider.Money{
			// available_balance 是 JSON number（float64）。FormatFloat('f', -1) 输出
			// 最短十进制表示，避免中间 float 运算引入精度误差。
			Amount:   strconv.FormatFloat(b.Data.AvailableBalance, 'f', -1, 64),
			Currency: "CNY",
		},
		UpdatedAt: time.Now(),
	}, nil
}

// balanceErrMsg 从 code != 0 的错误响应构造单 provider 错误信息。
// scode 如 exceeded_current_quota_error（余额耗尽）原样带入，便于前端识别。
func balanceErrMsg(b balanceResp) string {
	msg := strings.Join(strings.Fields(strings.TrimSpace(b.Message)), " ")
	scode := strings.Join(strings.Fields(strings.TrimSpace(b.Scode)), " ")
	switch {
	case scode != "" && msg != "":
		return scode + ": " + msg
	case scode != "":
		return scode
	case msg != "":
		return msg
	default:
		return fmt.Sprintf("上游业务码 %d", b.Code)
	}
}
