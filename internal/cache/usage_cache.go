// Package cache 提供基于内存的 TTL 用量缓存。
//
// 用途：避免每个 HTTP 请求都打 provider API（部分 provider 每次查询
// 消耗请求额度，如 modelscope）。缓存层保证：
//   - 命中未过期 → 直接返回，不打 provider；
//   - 未命中/过期 → singleflight 合并并发请求，只执行一次 fetch；
//   - fetch 失败 → 不写入缓存（可重试）；
//   - Invalidate/Clear 支持刷新场景主动清除。
package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"ai-usage/internal/provider"
)

// entry 是缓存中的单个条目。ttl 在写入时决定（forceTTL 覆盖默认），
// 缓存条目的生命周期不随后续调用参数变化。
// typ 是 fetch 返回的有效类型标记（"plan"/"balance"/"both"），供 GetOrFetchTyped 透传。
type entry struct {
	usage     *provider.Usage
	typ       string
	fetchedAt time.Time
	ttl       time.Duration
}

// typedResult 是 singleflight 的共享结果载体（usage + 有效类型标记）。
type typedResult struct {
	usage *provider.Usage
	typ   string
}

// Cache 是一个线程安全的内存 TTL 缓存。
//
// 并发安全模型：
//   - items 由 mu 保护（读写都持锁）；
//   - fetch 在锁外执行，避免持锁期间阻塞其他 key 的读写；
//   - 同一 key 的并发 fetch 由 singleflight.Group 合并为一次。
type Cache struct {
	mu         sync.Mutex
	items      map[string]*entry
	defaultTTL time.Duration
	group      singleflight.Group
}

// New 创建一个默认 TTL 为 defaultTTL 的缓存。
func New(defaultTTL time.Duration) *Cache {
	return &Cache{
		items:      make(map[string]*entry),
		defaultTTL: defaultTTL,
	}
}

// GetOrFetch 从缓存读取 key 对应的用量。
//
// 命中且未过期 → 直接返回缓存值；未命中或过期 → 调用 fetch 获取并写入缓存。
// forceTTL > 0 时覆盖默认 TTL（如 modelscope 强制 1h）。
//
// 并发语义：同一 key 的并发调用只执行一次 fetch，其余调用等待共享结果
// （singleflight）。fetch 返回错误时不写入缓存，下次调用会重新 fetch。
//
// key 由调用方保证唯一（如 provider+key 组合），本层不解析其含义。
func (c *Cache) GetOrFetch(ctx context.Context, key string, forceTTL time.Duration,
	fetch func(ctx context.Context) (*provider.Usage, error)) (*provider.Usage, error) {
	u, _, err := c.GetOrFetchTyped(ctx, key, forceTTL,
		func(ctx context.Context) (*provider.Usage, string, error) {
			usage, err := fetch(ctx)
			return usage, "", err
		})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetOrFetchTyped 与 GetOrFetch 相同，但 fetch 额外返回有效类型标记 typ，
// 该标记随 usage 一起缓存，命中缓存时原样返回（用于 server 层按 key 类型
// 标记（plan/balance/both）解析后的实际用量类型）。
func (c *Cache) GetOrFetchTyped(ctx context.Context, key string, forceTTL time.Duration,
	fetch func(ctx context.Context) (*provider.Usage, string, error)) (*provider.Usage, string, error) {

	ttl := c.defaultTTL
	if forceTTL > 0 {
		ttl = forceTTL
	}

	if e := c.get(key); e != nil && time.Since(e.fetchedAt) < e.ttl {
		return e.usage, e.typ, nil
	}

	// 未命中/过期：singleflight 合并并发 fetch。
	v, err, _ := c.group.Do(key, func() (interface{}, error) {
		// 双检查：等待 singleflight 期间可能已被其他调用写入。
		if e := c.get(key); e != nil && time.Since(e.fetchedAt) < e.ttl {
			return typedResult{usage: e.usage, typ: e.typ}, nil
		}
		u, typ, err := fetch(ctx)
		if err != nil {
			return typedResult{}, err
		}
		if u == nil {
			return typedResult{}, errors.New("cache: fetch returned nil usage without error")
		}
		c.set(key, &entry{usage: u, typ: typ, fetchedAt: time.Now(), ttl: ttl})
		return typedResult{usage: u, typ: typ}, nil
	})
	if err != nil {
		return nil, "", err
	}
	r := v.(typedResult)
	return r.usage, r.typ, nil
}

// Put 直接写入（或覆盖）指定 key 的缓存条目，ttl 为条目有效期。
//
// 用于单 key 强制刷新后把新结果同步到同账户兄弟 key 的缓存槽位，
// 避免下次批量查询时对同一账户重复打上游。不干预 singleflight 在途请求。
func (c *Cache) Put(key string, usage *provider.Usage, typ string, ttl time.Duration) {
	if usage == nil || ttl <= 0 {
		return
	}
	c.set(key, &entry{usage: usage, typ: typ, fetchedAt: time.Now(), ttl: ttl})
}

// Invalidate 主动清除指定 key 的缓存条目（刷新场景用）。
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	// 顺带遗忘 singleflight 中该 key 的进行中请求，
	// 保证 Invalidate 后新请求立即重新 fetch。
	c.group.Forget(key)
}

// Clear 清空全部缓存条目。
//
// 同时遗忘 singleflight 中这些 key 的进行中请求（与 Invalidate 同理）：
// 保证 Clear 后新请求立即重新 fetch，而非共享刷新前在途的旧结果。
func (c *Cache) Clear() {
	c.mu.Lock()
	keys := make([]string, 0, len(c.items))
	for k := range c.items {
		keys = append(keys, k)
	}
	c.items = make(map[string]*entry)
	c.mu.Unlock()
	// 锁外 Forget：singleflight.Group 内部自带 mutex，无需持 c.mu。
	for _, k := range keys {
		c.group.Forget(k)
	}
}

func (c *Cache) get(key string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[key]
}

func (c *Cache) set(key string, e *entry) {
	c.mu.Lock()
	c.items[key] = e
	c.mu.Unlock()
}
