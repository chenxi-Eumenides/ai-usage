// Package modelscope 实现 ModelScope 魔搭平台的 provider adapter。
//
// 本 adapter 只做「存储管理」，不做用量查询，原因如下（learnings T27）：
//  1. 外部来源中的 modelscope key 是 sk- 前缀——实测它是阿里云百炼
//     (DashScope) 的 key，不是 modelscope.cn（魔搭）的 ms- token。
//  2. api-inference.modelscope.cn 拒绝该 key（401/Invalid model id）；
//     dashscope.aliyuncs.com 接受（200）但不返回限流头，且无简单 API-key
//     用量查询端点（需阿里云 BSS AK/SK 签名）。
//  3. 因此废弃之前「发一次推理请求读 ratelimit 响应头」的方案（每次查询烧 1
//     次请求额度且拿不到稳定数据），改为与 mimo 相同的仅存储模式：
//     key 可录入/展示，fetch 返回 ErrNotSupported（按 kt 报不支持，归异常组，T44）。
//
// KeyPrefixes 返回 ["ms-", "sk-"]：ms- 是 modelscope.cn 官方 token 前缀，
// sk- 是实测存在的百炼 key 前缀。仅存储模式下前缀校验意义变小，两种都接受，
// 避免录入时因平台归属混淆而误拦截。
package modelscope

import (
	"context"

	"ai-usage/internal/provider"
)

// spec 是 ModelScope 魔搭的静态元数据（T41 数据驱动）。
var spec = provider.Spec{
	ID:          "modelscope",
	DisplayName: "ModelScope 魔搭",
	Aliases:     []string{"modelscope"},
	KeyPrefixes: []string{"ms-", "sk-"},
	ConsoleURL:  "https://modelscope.cn",
	UsageType:   provider.UsageTypePlan,
	UsageURLs:   provider.UsageURLs{Plan: "https://modelscope.cn/my"},
	PlanMeta: provider.PlanMeta{
		PlanName:  "ModelScope 免费 API",
		PlanPrice: "每日额度（次）",
		ApiPrice:  "免费",
	},
}

func init() {
	_ = provider.Register(spec, fetchUsage)
}

// fetchUsage 不支持用量查询，直接返回 ErrNotSupported。
// 详见包注释：实测 key 归属百炼（DashScope），魔搭无独立用量 API；
// 无论 keyType（plan/balance）均返回 ErrNotSupported。
func fetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	return nil, provider.ErrNotSupported
}
