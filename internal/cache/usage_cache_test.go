package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-usage/internal/provider"
)

func newUsage() *provider.Usage {
	return &provider.Usage{BalanceType: "balance"}
}

// TestHit：命中缓存时 fetch 只调 1 次。
func TestHit(t *testing.T) {
	c := New(5 * time.Minute)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return newUsage(), nil
	}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		u, err := c.GetOrFetch(ctx, "k1", 0, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u == nil {
			t.Fatal("got nil usage")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch called %d times, want 1", got)
	}
}

// TestExpiry：TTL 极短，过期后再次调用会重新 fetch。
func TestExpiry(t *testing.T) {
	c := New(1 * time.Millisecond)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return newUsage(), nil
	}

	ctx := context.Background()
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fetch called %d times, want 2", got)
	}
}

// TestForceTTL：forceTTL 覆盖默认 TTL（模拟 modelscope 强制 1h）。
func TestForceTTL(t *testing.T) {
	// 默认 TTL 极短，但第一次用 forceTTL=1h 写入 → 第二次仍在有效期内。
	c := New(1 * time.Millisecond)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return newUsage(), nil
	}

	ctx := context.Background()
	if _, err := c.GetOrFetch(ctx, "k1", time.Hour, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch called %d times, want 1 (forceTTL should override default)", got)
	}
}

// TestConcurrent：10 个 goroutine 并发同一 key → fetch 只调 1 次，无 data race。
func TestConcurrent(t *testing.T) {
	c := New(5 * time.Minute)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(10 * time.Millisecond) // 拉大竞争窗口
		return newUsage(), nil
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetOrFetch(ctx, "k1", 0, fetch)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch called %d times, want 1", got)
	}
}

// TestErrorNotCached：fetch 返回错误 → 不缓存，再次调用重新 fetch。
func TestErrorNotCached(t *testing.T) {
	c := New(5 * time.Minute)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("provider down")
	}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err == nil {
			t.Fatal("expected error, got nil")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fetch called %d times, want 2 (errors must not be cached)", got)
	}
}

// TestInvalidate：缓存后 Invalidate → 再次调用重新 fetch。
func TestInvalidate(t *testing.T) {
	c := New(5 * time.Minute)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return newUsage(), nil
	}

	ctx := context.Background()
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.Invalidate("k1")
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fetch called %d times, want 2 after Invalidate", got)
	}
}

func TestPut(t *testing.T) {
	c := New(5 * time.Minute)
	ctx := context.Background()
	u := &provider.Usage{BalanceType: "balance", Balance: &provider.Money{Amount: "9.99", Currency: "USD"}}

	c.Put("k1", u, "balance", time.Minute)
	got, typ, err := c.GetOrFetchTyped(ctx, "k1", 0, func(context.Context) (*provider.Usage, string, error) {
		t.Fatal("Put 后应命中缓存，不应触发 fetch")
		return nil, "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Balance.Amount != "9.99" || typ != "balance" {
		t.Errorf("Put 未写入正确条目: usage=%+v typ=%q", got, typ)
	}

	c.Put("k2", nil, "balance", time.Minute)
	if _, _, err := c.GetOrFetchTyped(ctx, "k2", 0, func(context.Context) (*provider.Usage, string, error) {
		return &provider.Usage{BalanceType: "balance"}, "", nil
	}); err != nil {
		t.Fatalf("nil usage 的 Put 应被忽略，走 fetch: %v", err)
	}
}

// TestClear：Clear 后再次调用重新 fetch。
func TestClear(t *testing.T) {
	c := New(5 * time.Minute)
	var calls int32
	fetch := func(ctx context.Context) (*provider.Usage, error) {
		atomic.AddInt32(&calls, 1)
		return newUsage(), nil
	}

	ctx := context.Background()
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.Clear()
	if _, err := c.GetOrFetch(ctx, "k1", 0, fetch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fetch called %d times, want 2 after Clear", got)
	}
}

// TestClearForgetsInFlight：Clear 时若该 key 有进行中的旧 fetch（如刷新前的在途数据），
// 新请求必须立即重新 fetch，不得共享旧在途结果（验证 Clear 需 Forget singleflight）。
func TestClearForgetsInFlight(t *testing.T) {
	c := New(30 * time.Millisecond)
	ctx := context.Background()

	// 先写入 v0，随后等其过期，制造「缓存中已有该 key + 新 fetch 在途」的刷新场景。
	if _, err := c.GetOrFetch(ctx, "k1", 0, func(context.Context) (*provider.Usage, error) {
		return &provider.Usage{BalanceType: "balance", Balance: &provider.Money{Amount: "v0"}}, nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.GetOrFetch(ctx, "k1", 0, func(context.Context) (*provider.Usage, error) {
			close(started)
			time.Sleep(500 * time.Millisecond) // 慢：模拟刷新前在途的旧数据
			return &provider.Usage{BalanceType: "balance", Balance: &provider.Money{Amount: "v1-stale"}}, nil
		})
	}()
	<-started

	c.Clear()

	// 新请求不应共享在途的 v1-stale，必须立即 fetch 到 v2。
	u, err := c.GetOrFetch(ctx, "k1", 0, func(context.Context) (*provider.Usage, error) {
		return &provider.Usage{BalanceType: "balance", Balance: &provider.Money{Amount: "v2"}}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := u.Balance.Amount; got != "v2" {
		t.Fatalf("got %q, want %q: new request shared stale in-flight fetch", got, "v2")
	}
	wg.Wait()
}
