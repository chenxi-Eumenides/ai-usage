// Package bailian 实现阿里云百炼（Model Studio / DashScope）Token Plan 的
// provider adapter。
//
// Token Plan 是百炼的大模型订阅套餐（Credits 统一计量），个人版按 5 小时 +
// 7 天窗口限额。与本项目其他 plan 类 provider 不同，百炼**没有开放任何
// 仅凭 API Key 的用量查询接口**（2026-08 调研结论）：
//
//   - 官方 OpenAPI 门户 ModelStudio/MaaS/bailian 产品全部接口均无 usage/quota
//     查询动作（仅 CreateApiKey 等管理面）；
//   - 阿里云账单侧 BssOpenApi（QueryAccountBalance 等）要求 RAM AK/SK HMAC
//     签名，与 sk- API Key 是两套完全独立的凭证体系，无法用 Key 查询；
//   - 实测套餐推理端点（compatible-mode /models、chat/completions）响应体
//     与响应头均不含剩余额度信息（仅 istio-envoy 计时头）。
//
// 官方唯一渠道是控制台「我的订阅」页查看 Credits 消耗，因此本 adapter 采用
// 与 mimo 相同的仅存储模式：key 可录入/展示（格式校验 + 控制台直达），
// fetch 返回 ErrNotSupported。
//
// key 格式：Token Plan 专属 API Key 以 sk-sp- 开头（官方文档确认），与按量
// 付费 sk-、Coding Plan sk-ws- 完全隔离不可混用。sk-sp key 为点分段不透明
// 字符串（如 sk-sp-H.XXXXXX.YYYY.MEUCIQ...，尾段疑似 base64url DER 签名，
// 长度不固定），官方社区实现（qwen-code/one-api）均不做结构解析，故仅做
// 前缀 + 字符集 + 最小长度校验，不硬编码分段数与总长度。
//
// 推理端点（供接入工具参考，本网关不代理推理流量）：
// OpenAI 兼容 https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
// Anthropic 兼容 https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic
package bailian

import (
	"context"

	"ai-usage/internal/provider"
)

// keyPattern 是百炼 Token Plan key 格式：sk-sp- 前缀 + 不透明主体
// （点分段 base64url 风格字符集，最小 16 位防误粘贴/截断）。
// RE2 兼容；api.go keyMatchesProvider 在 pattern 非空时完整匹配此表达式。
const keyPattern = `^sk-sp-[A-Za-z0-9._~-]{16,}$`

// usagePageURL 是控制台 Token Plan 个人版「我的订阅」用量页直达链接。
const usagePageURL = "https://bailian.console.aliyun.com/cn-beijing?tab=plan&commonbuy=1&orderType=buy#/efm/subscription/token-plan/personal"

// spec 是阿里云百炼的静态元数据（T41 数据驱动）。
// 仅存储模式：UsageNote 提供「用量说明」兜底文案（无用量页的 keyType 用）。
var spec = provider.Spec{
	ID:          "bailian",
	DisplayName: "阿里云百炼",
	Aliases:     []string{"alibaba", "alibaba-cn", "bailian", "dashscope"},
	// Token Plan 专属 sk-sp- 前缀 key（点分多段，前缀匹配不够精确，用正则）。
	KeyPattern: keyPattern,
	ConsoleURL: "https://bailian.console.aliyun.com",
	UsageType:  provider.UsageTypePlan,
	UsageURLs: provider.UsageURLs{
		// 百炼 Token Plan 是订阅套餐，无按量余额概念（额度耗尽即停服，
		// 不转按量扣费），balance 类型无对应页面 → 留空。
		Plan: usagePageURL,
	},
	UsageNote: "百炼 Token Plan 官方未开放用量查询 API（OpenAPI 与 Bss 均需阿里云 AK/SK 签名），无法在线获取 Credits 用量；" +
		"请在控制台「我的订阅」页查看额度与消耗。",
	PlanMeta: provider.PlanMeta{
		PlanName:  "百炼 Token Plan",
		PlanPrice: "官方订阅（best-effort）",
		// Token Plan 无按量价：套餐额度独立于账户余额，耗尽即停。
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// fetchUsage 不支持用量查询，直接返回 ErrNotSupported。
// 官方无 API-key 可用的用量查询端点（调研依据见包注释）；
// 两通道均返回 ErrNotSupported，key 无论标记类型一律归仪表盘异常组，
// 报错「此提供商不支持X查询」，卡片带用量页直达/说明按钮（T44）。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	return nil, provider.ErrNotSupported
}
