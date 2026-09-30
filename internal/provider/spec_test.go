package provider

import "testing"

// TestSpecUsageURL 验证 UsageURL 解析规则（T40 契约）：
// plan/balance 直取对应字段；both 无专属页面，按主类型 UsageType 取页。
func TestSpecUsageURL(t *testing.T) {
	planPrimary := Spec{
		UsageType: UsageTypePlan,
		UsageURLs: UsageURLs{Plan: "https://p.example/plan", Balance: "https://p.example/balance"},
	}
	if got := planPrimary.UsageURL(UsageTypePlan); got != "https://p.example/plan" {
		t.Errorf("plan: got %q", got)
	}
	if got := planPrimary.UsageURL(UsageTypeBalance); got != "https://p.example/balance" {
		t.Errorf("balance: got %q", got)
	}
	if got := planPrimary.UsageURL("both"); got != "https://p.example/plan" {
		t.Errorf("both（主类型 plan）: got %q, want 套餐页", got)
	}

	balancePrimary := Spec{
		UsageType: UsageTypeBalance,
		UsageURLs: UsageURLs{Plan: "https://b.example/plan", Balance: "https://b.example/balance"},
	}
	if got := balancePrimary.UsageURL("both"); got != "https://b.example/balance" {
		t.Errorf("both（主类型 balance）: got %q, want 余额页", got)
	}

	// 次类型无页面：返回空串（该类 key 归异常组，前端弹「用量说明」，T44）
	planOnly := Spec{UsageType: UsageTypePlan, UsageURLs: UsageURLs{Plan: "https://x/plan"}}
	if got := planOnly.UsageURL(UsageTypeBalance); got != "" {
		t.Errorf("balance 侧无页面应返回空串, got %q", got)
	}
}
