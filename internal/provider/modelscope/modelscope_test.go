package modelscope

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ai-usage/internal/provider"
)

// TestNotSupported 验证无论 keyType（plan/balance）FetchUsage 都返回 ErrNotSupported。
func TestUsageConsoleURL(t *testing.T) {
	want := "https://modelscope.cn/my"
	if got := spec.UsageURL(provider.UsageTypePlan); got != want {
		t.Errorf("plan: got %q, want %q", got, want)
	}
	if got := spec.UsageURL(provider.UsageTypeBalance); got != "" {
		t.Errorf("balance: 无对应页面，应返回空串, got %q", got)
	}
	if got := spec.UsageURL("both"); got != want {
		t.Errorf("both: got %q, want %q", got, want)
	}
}

func TestNotSupported(t *testing.T) {
	for _, keyType := range []string{provider.UsageTypePlan, provider.UsageTypeBalance} {
		_, err := fetchUsage(context.Background(), "sk-test-modelscope", keyType)
		if err == nil {
			t.Fatalf("keyType=%s: expected error for unsupported usage query", keyType)
		}
		if !errors.Is(err, provider.ErrNotSupported) {
			t.Errorf("keyType=%s: expected ErrNotSupported, got %v", keyType, err)
		}
		if !provider.IsNotSupported(err) {
			t.Errorf("keyType=%s: expected IsNotSupported(err)==true, got false", keyType)
		}
	}
}

// TestMetadata 验证 spec 元数据正确 + init() 自注册链路。
func TestMetadata(t *testing.T) {
	if got := spec.ID; got != "modelscope" {
		t.Errorf("expected ID=modelscope, got %q", got)
	}
	if got := spec.DisplayName; got != "ModelScope 魔搭" {
		t.Errorf("expected DisplayName=ModelScope 魔搭, got %q", got)
	}
	if !reflect.DeepEqual(spec.Aliases, []string{"modelscope"}) {
		t.Errorf("expected Aliases=[modelscope], got %v", spec.Aliases)
	}
	if !reflect.DeepEqual(spec.KeyPrefixes, []string{"ms-", "sk-"}) {
		t.Errorf("expected KeyPrefixes=[ms-, sk-], got %v", spec.KeyPrefixes)
	}
	if got := spec.ConsoleURL; got != "https://modelscope.cn" {
		t.Errorf("expected ConsoleURL=https://modelscope.cn, got %q", got)
	}
	if meta := spec.PlanMeta; meta.PlanName != "ModelScope 免费 API" {
		t.Errorf("expected PlanName=ModelScope 免费 API, got %q", meta.PlanName)
	}
}
