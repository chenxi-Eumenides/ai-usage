package provider

// Spec 是 provider 的静态元数据（T41 数据驱动）：名称/别名/前缀/控制台/
// 用量页链接/用量说明/套餐元数据全部是数据字段，不含逻辑。
// 新增提供商 = 一条 Spec 数据 + 一个 FetchFunc（注册见 registry.go）。
type Spec struct {
	ID          string   // 唯一标识，如 "deepseek"
	DisplayName string   // 展示名，如 "DeepSeek"
	Aliases     []string // 外部来源中的 provider 名映射，如 ["kimi-for-coding"]
	// KeyPrefixes key 格式前缀校验列表，如 ["sk-"]；nil 表示不校验。
	KeyPrefixes []string
	// KeyPattern key 格式正则校验（完整匹配）；非空时优先于 KeyPrefixes，
	// 用于前缀无法表达的格式（如智谱 32hex.16alnum）。
	KeyPattern string
	ConsoleURL string // 控制台直达链接
	// ProxyURL 是该 provider 出站请求的显式 HTTP(S)/SOCKS5 代理；空串表示直连。
	// 该配置不会继承 HTTP_PROXY/HTTPS_PROXY，避免一个 provider 的代理影响其他 provider。
	ProxyURL string
	// UsageType 主查询类型：UsageTypePlan 或 UsageTypeBalance。
	// 前端按它归类仪表盘卡片，并在 both 场景决定 UsageURL 取哪一侧。
	UsageType string
	// UsageURLs 按类型给出控制台「用量页」直达链接；该类型无对应页面留空。
	// T40 契约：能产出数据的类型一律非空；空串仅出现在无查询实现的类型上
	//（T44 起此类 key 归异常组渲染，空串走前端「用量说明」弹框路径）。
	UsageURLs UsageURLs
	// UsageNote 无用量页时的「用量说明」弹框文案（可空）。
	// 前端在 UsageConsoleURL 返回空串时展示弹框：有 UsageNote 用之，
	// 否则用前端通用文案。
	UsageNote string
	PlanMeta  PlanMeta // 静态套餐元数据（best-effort，可为零值）
	// Credential 非 nil 表示 provider 使用账号级手动凭证。
	Credential *CredentialSpec
}

// CredentialLink 是凭证录入说明中的外部链接。
type CredentialLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// CredentialSpec 声明 provider 所需的手动凭证元数据。
type CredentialSpec struct {
	Kind        string           `json:"kind"`
	Label       string           `json:"label"`
	Help        string           `json:"help"`
	Placeholder string           `json:"placeholder"`
	Links       []CredentialLink `json:"links"`
	KeyFallback bool             `json:"-"`
}

// UsageURLs 按 key 类型组织「用量页」链接。
type UsageURLs struct {
	Plan    string // 套餐用量页
	Balance string // 余额用量页
}

// UsageURL 按 keyType 解析「用量页」直达链接。
// plan/balance 直取对应字段；both 无专属页面，按主类型 UsageType 取页
// （如 openai 主类型 plan → both 取套餐分析页）。
func (s Spec) UsageURL(keyType string) string {
	switch keyType {
	case UsageTypePlan:
		return s.UsageURLs.Plan
	case UsageTypeBalance:
		return s.UsageURLs.Balance
	default: // both
		if s.UsageType == UsageTypeBalance {
			return s.UsageURLs.Balance
		}
		return s.UsageURLs.Plan
	}
}
