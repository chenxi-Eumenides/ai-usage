package provider

import (
	"encoding/json"
	"time"
)

// Usage 是统一的全量用量查询结果。
//
// 一个 Usage 对应一个 provider 的查询结果。字段设计遵循「单 struct + 可选字段」
// 原则：不存在的维度用 nil 表达，不做类型层级。
//
// 所有数字字段一律使用 string 类型，避免 float 精度问题，金额/配额直接透传
// provider 原始响应，不做计算与格式化（格式化在展示层完成）。
type Usage struct {
	// BalanceType 余额类型："balance"（按量余额）| "quota"（套餐配额）| ""（未知）。
	// 一次查询只能命中一种主类型；同一 provider 同时有余额和配额窗口时，
	// Balance 与 Quotas 可以并存（如 opencode-go 降级场景）。
	BalanceType string          `json:"balanceType"`
	Balance     *Money          `json:"balance,omitempty"`   // 按量余额，仅 balance 类 provider
	Quotas      []Quota         `json:"quotas,omitempty"`    // 配额窗口列表（5h/weekly/monthly/daily）
	Plan        *PlanInfo       `json:"plan,omitempty"`      // 套餐等级（API 动态获取）
	Error       string          `json:"error,omitempty"`     // 单 provider 失败信息（HTTP 仍 200 的局部失败）
	ErrorCode   string          `json:"errorCode,omitempty"` // 可供调用方识别的稳定错误码
	Raw         json.RawMessage `json:"raw,omitempty"`       // 原始响应（可选展示）
	UpdatedAt   time.Time       `json:"updatedAt"`           // 查询时间（RFC3339）
}

// Money 表示一笔金额。Amount 为字符串，避免 float 精度损失。
type Money struct {
	Amount   string `json:"amount"`   // 如 "12.34"
	Currency string `json:"currency"` // "CNY" | "USD"
}

// Quota 表示一个配额窗口的使用情况。多个 provider（kimi/zai/opencode-go）
// 都会返回多个窗口，因此是列表。
type Quota struct {
	Period    string    `json:"period"`            // "5h" | "weekly" | "monthly" | "daily"
	Used      string    `json:"used"`              // 已用量
	Limit     string    `json:"limit"`             // 窗口上限
	Remaining string    `json:"remaining"`         // 剩余量
	Percent   string    `json:"percent,omitempty"` // 剩余百分比（部分 API 直接给出）
	ResetAt   time.Time `json:"resetAt,omitempty"` // 重置时间，零值表示无
}

// PlanInfo 是套餐等级的动态查询结果（来自 provider API）。
type PlanInfo struct {
	Level string `json:"level"` // 套餐等级，如 "LEVEL_INTERMEDIATE"
}

// PlanMeta 是套餐的静态元数据，由 adapter 写死（best-effort）。
// 与 PlanInfo 的区别：PlanMeta 是静态的（adapter 内置），
// PlanInfo 是动态的（API 返回）。
type PlanMeta struct {
	PlanName  string // 静态套餐名
	PlanPrice string // 静态价格（best-effort）
	ApiPrice  string // 静态按量价（best-effort）
}
