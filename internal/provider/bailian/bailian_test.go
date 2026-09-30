package bailian

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"ai-usage/internal/provider"
)

// TestNotSupported 验证无论 keyType（plan/balance）FetchUsage 都返回 ErrNotSupported。
func TestNotSupported(t *testing.T) {
	for _, keyType := range []string{provider.UsageTypePlan, provider.UsageTypeBalance} {
		_, err := fetchUsage(context.Background(), "sk-sp-H.ABCDEF.GHIJ.MEUCIQaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", keyType)
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

// TestKeyPattern 验证 sk-sp 套餐 key 正则：合法格式通过，误用格式拒绝。
func TestKeyPattern(t *testing.T) {
	re := regexp.MustCompile(keyPattern)

	valid := []string{
		// 实测格式：4 段点分 + base64url DER 签名尾（含 _ 字符），此处为同构合成样本。
		"sk-sp-H.SYNTHT.SAMP.MEUCIQaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sk-sp-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789._-~",
	}
	for _, key := range valid {
		if !re.MatchString(key) {
			t.Errorf("expected valid key to match: %q", key)
		}
	}

	invalid := []string{
		"",
		"sk-sp-",                              // 只有前缀
		"sk-sp-H.DRM",                         // 主体过短（截断/脱敏残片）
		"sk-2c6c4f1a8b9d3e5f7a1c3d5e7f9a1b3d", // 按量付费 sk- key（不可混用）
		"sk-ws-ABCDEFGHIJKLMNOPQRSTUVWX",      // Coding Plan key（不可混用）
		"sk-sp-H.SYNTHT.SAMP.MEUCIQaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ", // 尾随空格
		"sk-sp-key+with+plus",        // '+' 不在 base64url/点分字符集
		"ms-somethinglongenough1234", // 他平台前缀
	}
	for _, key := range invalid {
		if re.MatchString(key) {
			t.Errorf("expected invalid key to be rejected: %q", key)
		}
	}
}

// TestMetadata 验证 spec 元数据正确 + init() 自注册链路。
func TestMetadata(t *testing.T) {
	if got := spec.ID; got != "bailian" {
		t.Errorf("expected ID=bailian, got %q", got)
	}
	if got := spec.DisplayName; got != "阿里云百炼" {
		t.Errorf("expected DisplayName=阿里云百炼, got %q", got)
	}
	if spec.KeyPattern != keyPattern {
		t.Errorf("expected KeyPattern=keyPattern, got %q", spec.KeyPattern)
	}
	if len(spec.KeyPrefixes) != 0 {
		t.Errorf("KeyPrefixes should be empty when KeyPattern is set, got %v", spec.KeyPrefixes)
	}
	if got := spec.ConsoleURL; got != "https://bailian.console.aliyun.com" {
		t.Errorf("expected ConsoleURL=https://bailian.console.aliyun.com, got %q", got)
	}
	if got := spec.UsageType; got != provider.UsageTypePlan {
		t.Errorf("expected UsageType=plan, got %q", got)
	}
	if spec.UsageNote == "" {
		t.Error("expected non-empty UsageNote（官方无用量 API，弹框兜底文案）")
	}

	p, ok := provider.Get("bailian")
	if !ok {
		t.Fatal("bailian not registered via init()")
	}
	if p.UsageNote() != spec.UsageNote {
		t.Errorf("UsageNote mismatch: got %q", p.UsageNote())
	}
	if p.KeyPattern() != keyPattern {
		t.Errorf("KeyPattern passthrough mismatch: got %q", p.KeyPattern())
	}

	// 外部来源中的 provider 别名映射链路
	for _, alias := range []string{"alibaba", "alibaba-cn", "bailian", "dashscope"} {
		if got, ok := provider.GetByAlias(alias); !ok || got.ID() != "bailian" {
			t.Errorf("alias %q: expected resolve to bailian, got ok=%v", alias, ok)
		}
	}
}

// TestUsageConsoleURL 验证 T40 契约：plan/both 给控制台用量页，balance 无页面留空。
func TestUsageConsoleURL(t *testing.T) {
	if got := spec.UsageURL(provider.UsageTypePlan); got != usagePageURL {
		t.Errorf("plan: got %q, want 我的订阅用量页", got)
	}
	// 主类型 plan → both 按主类型取套餐页（非空，前端渲染直达按钮）。
	if got := spec.UsageURL("both"); got != usagePageURL {
		t.Errorf("both: got %q, want 套餐用量页（主类型 plan）", got)
	}
	// 百炼无按量余额概念 → balance 留空，前端弹「用量说明」。
	if got := spec.UsageURL(provider.UsageTypeBalance); got != "" {
		t.Errorf("balance: got %q, want empty（无余额页）", got)
	}
}
