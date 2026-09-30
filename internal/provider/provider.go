package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	errInvalidProxyScheme = errors.New("proxy URL scheme must be http, https, or socks5")
	errMissingProxyHost   = errors.New("proxy URL must include a host")
)

// UsageTypePlan / UsageTypeBalance 是 UsageType() 的合法返回值。
//
// "plan" 表示 provider 主查询套餐类用量（kimi-code/minimax/zai/opencode-go/modelscope/mimo 等）；
// "balance" 表示 provider 主查询余额类用量（deepseek/moonshot-cn 等）。
//
// 该值用于前端按类目聚合仪表盘卡片（learnings T22）：用户希望把同类 provider
// 分组展示，即使 fetch 失败（usage 为 nil）前端也能根据 UsageType 决定卡片归属。
const (
	UsageTypePlan    = "plan"
	UsageTypeBalance = "balance"
)

// proxyContextKey 是 specAdapter 为单个请求注入显式代理时使用的上下文键。
type proxyContextKey struct{}

// withProxyURL 将已经由 Register 校验过的代理 URL 附加到请求上下文。
func withProxyURL(ctx context.Context, proxyURL *url.URL) context.Context {
	if proxyURL == nil {
		return ctx
	}
	return context.WithValue(ctx, proxyContextKey{}, proxyURL)
}

func proxyURLFromContext(ctx context.Context) *url.URL {
	proxyURL, _ := ctx.Value(proxyContextKey{}).(*url.URL)
	return proxyURL
}

// contextProxyTransport 对每个显式代理维护一个独立 Transport。
// 未带代理的请求始终经 direct 直连，不会读取环境变量代理。
type contextProxyTransport struct {
	direct  *http.Transport
	proxies sync.Map // map[string]*http.Transport
}

func newContextProxyTransport() *contextProxyTransport {
	return &contextProxyTransport{direct: &http.Transport{Proxy: nil}}
}

func (t *contextProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxyURL := proxyURLFromContext(req.Context())
	if proxyURL == nil {
		return t.direct.RoundTrip(req)
	}

	key := proxyURL.String()
	transport, ok := t.proxies.Load(key)
	if !ok {
		candidate := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		transport, _ = t.proxies.LoadOrStore(key, candidate)
	}
	return transport.(*http.Transport).RoundTrip(req)
}

// parseProxyURL 校验 Spec 中的显式代理地址。net/http 原生支持 HTTP(S) 和 SOCKS5。
func parseProxyURL(rawURL string) (*url.URL, error) {
	if rawURL == "" {
		return nil, nil
	}

	proxyURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" && proxyURL.Scheme != "socks5" {
		return nil, &url.Error{Op: "parse", URL: rawURL, Err: errInvalidProxyScheme}
	}
	if proxyURL.Hostname() == "" {
		return nil, &url.Error{Op: "parse", URL: rawURL, Err: errMissingProxyHost}
	}
	return proxyURL, nil
}

// NewHTTPClient 构造按请求上下文选择显式代理、默认禁用代理的 http.Client。
//
// 默认 http.Client 会读取环境变量 HTTPS_PROXY/HTTP_PROXY 并通过
// http.ProxyFromEnvironment 走代理。用户 shell 常配置科学上网代理
// （如 HTTPS_PROXY=http://127.0.0.1:20171/），网关进程继承该环境变量后
// 所有 provider 请求都会被代理转发；实测该代理对 opencode.ai 等境外
// 域名 CONNECT 处理失败（HTTP 000 / EOF，learnings T35）。provider 官方
// API 直连更快更稳，因此默认 Transport{Proxy: nil} 禁用代理。只有 provider
// 的 Spec.ProxyURL 明确配置时，specAdapter 才会为该请求启用独立代理 Transport。
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: newContextProxyTransport(),
	}
}

// Provider 是所有 AI 提供商 adapter 的统一接口。
// T41 数据驱动：元数据方法由 registry 内的 specAdapter 按 Spec 数据统一实现，
// 各提供商只需提供 Spec 数据 + FetchFunc 查询函数（init() 自注册 + providers.go
// 加 blank import）。未来新增平台 = 一条 Spec 数据 + 一个 fetch 函数。
type Provider interface {
	// ID 返回唯一标识，如 "deepseek"。
	ID() string

	// DisplayName 返回展示名，如 "DeepSeek"。
	DisplayName() string

	// Aliases 返回外部来源中的 provider 名映射列表。
	// 如 deepseek 有 ["deepseek"]，kimi 有 ["kimi-for-coding"]。
	Aliases() []string

	// KeyPrefixes 返回 key 格式前缀校验列表。
	// 如 deepseek 返回 ["sk-"]，modelscope 返回 ["ms-", "sk-"]（仅存储，两种都接受）。
	// 返回 nil 表示不校验前缀（fallback 场景）。
	KeyPrefixes() []string

	// KeyPattern 返回 key 格式正则（完整匹配）；空串表示无正则校验。
	// 非空时优先于 KeyPrefixes，用于前缀无法表达的格式（如智谱 32hex.16alnum）。
	KeyPattern() string

	// ConsoleURL 返回控制台直达链接。
	ConsoleURL() string

	// UsageConsoleURL 返回该 provider 指定 key 类型（plan/balance/both）对应的
	// 控制台「用量页」直达链接（T41 数据驱动：实现来自 Spec.UsageURL，both 按
	// 主类型 UsageType 取页）。能产出数据的 keyType 一律非空；空串仅当该类型
	// 无对应页面，此时前端应展示 UsageNote 说明弹框而非直接跳转。
	UsageConsoleURL(keyType string) string

	// UsageNote 返回无用量页时的「用量说明」文案（可空）。
	// 前端在 UsageConsoleURL 为空时弹框展示：有文案用之，空串用通用文案。
	UsageNote() string

	// PlanMeta 返回静态套餐元数据（adapter 内置，可为零值）。
	PlanMeta() PlanMeta

	// UsageType 返回该 provider 主查询类型：UsageTypePlan 或 UsageTypeBalance。
	// 用于前端按类目分组仪表盘卡片。该值与具体一次 fetch 的结果无关——
	// 即使 fetch 失败/返回错误，前端也能依据 UsageType 决定卡片归类。
	UsageType() string

	// FetchUsage 按指定类型查询用量。credential 在 key 模式下为 API key，
	// 在 cookie 模式下为账号级 Cookie 请求头原文。
	//
	// keyType 取值：UsageTypePlan（套餐）或 UsageTypeBalance（余额）。
	// 若 provider 不支持该类型查询，返回 ErrNotSupported（调用方据此忽略或合并）。
	// 例：deepseek 只支持 balance → keyType=UsageTypePlan 时返回 ErrNotSupported。
	//
	// server 层按 key 的类型标记（plan/balance/both）调度：both 时两个类型都尝试，
	// ErrNotSupported 一侧跳过；单类型 key 遇 ErrNotSupported 按 kt 报「不支持X查询」，
	// 归异常组显示（T44，不再隐藏）。
	FetchUsage(ctx context.Context, credential string, keyType string) (*Usage, error)
}
