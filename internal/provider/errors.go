package provider

import "errors"

// 错误分类（sentinel errors）。adapter 用 fmt.Errorf("%w: ...", ErrX) 包装，
// 调用方用 errors.Is 识别。
var (
	// ErrNotSupported 表示该 provider 不支持用量查询（如 mimo 无 API）。
	ErrNotSupported = errors.New("usage query not supported")
	// ErrAuth 表示认证失败（key 无效、过期等）。
	ErrAuth = errors.New("authentication failed")
	// ErrRateLimit 表示请求被限流。
	ErrRateLimit = errors.New("rate limited")
	// ErrParse 表示响应解析失败（结构不符、字段缺失等）。
	ErrParse = errors.New("response parse failed")
	// ErrUpstream 表示上游以非成功状态响应。
	ErrUpstream = errors.New("upstream unavailable")
	// ErrRetriesExhausted 表示网络请求重试后仍未成功。
	ErrRetriesExhausted = errors.New("upstream retries exhausted")
)

// IsAuthError 判断错误是否为认证失败（含被包装的情况）。
func IsAuthError(err error) bool { return errors.Is(err, ErrAuth) }

// IsRateLimited 判断错误是否为限流。
func IsRateLimited(err error) bool { return errors.Is(err, ErrRateLimit) }

// IsParseError 判断错误是否为解析失败。
func IsParseError(err error) bool { return errors.Is(err, ErrParse) }

// IsNotSupported 判断错误是否为「不支持用量查询」。
func IsNotSupported(err error) bool { return errors.Is(err, ErrNotSupported) }
