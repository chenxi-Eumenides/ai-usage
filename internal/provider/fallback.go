package provider

import "context"

// fallbackProvider 用于未知 provider 名（外部来源中有但无对应 adapter）。
// FetchUsage 返回 ErrNotSupported（仅存储，不支持用量查询）。
type fallbackProvider struct{ id string }

func (f fallbackProvider) ID() string                            { return f.id }
func (f fallbackProvider) DisplayName() string                   { return f.id }
func (f fallbackProvider) Aliases() []string                     { return nil }
func (f fallbackProvider) KeyPrefixes() []string                 { return nil }
func (f fallbackProvider) KeyPattern() string                    { return "" }
func (f fallbackProvider) ConsoleURL() string                    { return "" }
func (f fallbackProvider) UsageConsoleURL(keyType string) string { return "" }
func (f fallbackProvider) UsageNote() string                     { return "" }
func (f fallbackProvider) PlanMeta() PlanMeta                    { return PlanMeta{} }
func (f fallbackProvider) UsageType() string                     { return UsageTypePlan }

// FetchUsage 始终返回 ErrNotSupported（未知 provider 无法查询用量）。
func (f fallbackProvider) FetchUsage(ctx context.Context, key string, keyType string) (*Usage, error) {
	return nil, ErrNotSupported
}

// Fallback 构造一个未知 provider 的 fallback adapter。
// 用于外部来源中存在但无对应 adapter 的 provider。
func Fallback(id string) Provider {
	return fallbackProvider{id: id}
}
