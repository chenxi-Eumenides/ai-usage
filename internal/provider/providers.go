package provider

// 各 adapter 通过 init() 自注册。
//
// ⚠️ 注册聚合点不在本包：provider 包直接 blank import 子 adapter 会导致
// import cycle（adapter 需 import 本包获取 Register/类型/sentinel）。
// 聚合入口见 cmd/gateway/main.go（database/sql 驱动注册模式）：
//
//	_ "ai-usage/internal/provider/bailian"
//	_ "ai-usage/internal/provider/deepseek"
//	_ "ai-usage/internal/provider/kimi"
//	_ "ai-usage/internal/provider/kimicode"
//	_ "ai-usage/internal/provider/moonshot"
//	_ "ai-usage/internal/provider/minimax"
//	_ "ai-usage/internal/provider/zai"
//	_ "ai-usage/internal/provider/opencodego"
//	_ "ai-usage/internal/provider/modelscope"
//	_ "ai-usage/internal/provider/mimo"
//	_ "ai-usage/internal/provider/openai"
