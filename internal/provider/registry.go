package provider

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"sync"
)

// FetchFunc 是 provider 的用量查询逻辑——Provider 接口中唯一非数据的部分。
// 签名与 Provider.FetchUsage 一致；不支持的 keyType 返回 ErrNotSupported。
type FetchFunc func(ctx context.Context, credential string, keyType string) (*Usage, error)

// specAdapter 用 Spec 实现 Provider 的全部元数据方法，fetch 提供查询逻辑。
// 元数据零逻辑：所有方法都是 Spec 字段的直取/转发。
type specAdapter struct {
	spec     Spec
	proxyURL *url.URL
	fetch    FetchFunc
}

func (a specAdapter) ID() string                            { return a.spec.ID }
func (a specAdapter) DisplayName() string                   { return a.spec.DisplayName }
func (a specAdapter) Aliases() []string                     { return a.spec.Aliases }
func (a specAdapter) KeyPrefixes() []string                 { return a.spec.KeyPrefixes }
func (a specAdapter) KeyPattern() string                    { return a.spec.KeyPattern }
func (a specAdapter) ConsoleURL() string                    { return a.spec.ConsoleURL }
func (a specAdapter) UsageConsoleURL(keyType string) string { return a.spec.UsageURL(keyType) }
func (a specAdapter) UsageNote() string                     { return a.spec.UsageNote }
func (a specAdapter) PlanMeta() PlanMeta                    { return a.spec.PlanMeta }
func (a specAdapter) UsageType() string                     { return a.spec.UsageType }
func (a specAdapter) CredentialSpec() *CredentialSpec {
	if a.spec.Credential == nil {
		return nil
	}
	credential := *a.spec.Credential
	credential.Links = append([]CredentialLink(nil), a.spec.Credential.Links...)
	return &credential
}

func (a specAdapter) FetchUsage(ctx context.Context, key string, keyType string) (*Usage, error) {
	return a.fetch(withProxyURL(ctx, a.proxyURL), key, keyType)
}

var (
	mu        sync.RWMutex
	providers = make(map[string]Provider) // id → provider
	aliases   = make(map[string]string)   // alias → provider id
)

// Register 注册一个 provider：静态元数据 Spec + 查询逻辑 fetch。
// ID 为空、fetch 为 nil 或 id 已注册时返回错误。
// 初始化阶段单 goroutine 调用，但使用写锁以保证后续并发安全。
func Register(spec Spec, fetch FetchFunc) error {
	mu.Lock()
	defer mu.Unlock()

	if spec.ID == "" {
		return fmt.Errorf("provider spec 缺少 ID")
	}
	if fetch == nil {
		return fmt.Errorf("provider %q 缺少 fetch 函数", spec.ID)
	}
	if spec.Credential != nil && spec.Credential.Kind != "cookie" && spec.Credential.Kind != "token" {
		return fmt.Errorf("provider %q 的凭证 Kind 无效: %q", spec.ID, spec.Credential.Kind)
	}
	proxyURL, err := parseProxyURL(spec.ProxyURL)
	if err != nil {
		return fmt.Errorf("provider %q 的 ProxyURL 无效: %w", spec.ID, err)
	}
	if _, ok := providers[spec.ID]; ok {
		return fmt.Errorf("provider %q already registered", spec.ID)
	}
	providers[spec.ID] = specAdapter{spec: spec, proxyURL: proxyURL, fetch: fetch}
	for _, alias := range spec.Aliases {
		aliases[alias] = spec.ID
	}
	return nil
}

// Get 按 id 查找已注册的 provider。
func Get(id string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := providers[id]
	return p, ok
}

// GetByAlias 按外部来源中的 provider 名别名查找已注册的 provider。
func GetByAlias(alias string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	id, ok := aliases[alias]
	if !ok {
		return nil, false
	}
	p, ok := providers[id]
	return p, ok
}

// All 返回全部已注册的 provider，按 id 排序以保证稳定顺序。
func All() []Provider {
	mu.RLock()
	defer mu.RUnlock()
	result := make([]Provider, 0, len(providers))
	for _, p := range providers {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID() < result[j].ID()
	})
	return result
}
