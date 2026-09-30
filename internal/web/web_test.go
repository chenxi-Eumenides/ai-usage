package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/appconf"
	"ai-usage/internal/cache"
	"ai-usage/internal/provider"
	"ai-usage/internal/server"
	"ai-usage/internal/store"
)

type stubProvider struct {
	id      string
	display string
	console string
	err     error
}

func (s *stubProvider) ID() string                            { return s.id }
func (s *stubProvider) DisplayName() string                   { return s.display }
func (s *stubProvider) Aliases() []string                     { return []string{s.id} }
func (s *stubProvider) KeyPrefixes() []string                 { return nil }
func (s *stubProvider) KeyPattern() string                    { return "" }
func (s *stubProvider) ConsoleURL() string                    { return s.console }
func (s *stubProvider) UsageConsoleURL(keyType string) string { return "" }
func (s *stubProvider) UsageNote() string                     { return "" }
func (s *stubProvider) UsageType() string                     { return provider.UsageTypeBalance }
func (s *stubProvider) PlanMeta() provider.PlanMeta {
	return provider.PlanMeta{PlanName: s.id + " Plan"}
}
func (s *stubProvider) FetchUsage(ctx context.Context, key string, keyType string) (*provider.Usage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &provider.Usage{
		BalanceType: "balance",
		Balance:     &provider.Money{Amount: "99.50", Currency: "CNY"},
		UpdatedAt:   time.Now(),
	}, nil
}

func newTestHandler(t *testing.T, appConfigs ...appconf.Config) *Handler {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := server.NewServer(st, cache.New(5*time.Minute), 5*time.Minute, appConfigs...)
	stub := &stubProvider{id: "stub", display: "Stub Provider", console: "https://stub.example"}
	s.Lookup = func(id string) (provider.Provider, bool) {
		if id == "stub" {
			return stub, true
		}
		return nil, false
	}
	return NewHandler(s)
}

func doGET(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRenderTemplateBasic(t *testing.T) {
	h := newTestHandler(t)

	w := doGET(h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	wantSubstrings := []string{
		"<!DOCTYPE html>",
		"AI 用量助手",
		"用量仪表盘",
		"Key 管理",
		`id="btn-add"`,
		`id="add-form"`,
		`id="add-modal"`,
		`id="btn-refresh"`,
		`id="dashboard"`,
		`id="keys-table"`,
		"application/json",
		`id="dashboard-empty"`,
		`id="edit-modal"`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(body, s) {
			t.Errorf("HTML 缺少 %q", s)
		}
	}

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html 前缀", ct)
	}
}

// TestRenderTemplateClassifyAndSortExists 验证前端核心分类排序逻辑存在。
func TestRenderTemplateClassifyAndSortExists(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		"function classifyAndSort",
		"套餐用量",
		"余额用量",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("模板 JS 缺少 %q", s)
		}
	}
}

// TestRenderTemplateFmtQuotaPercentOnly 守护 T28 前端修复：fmtQuota 不再拼接
// period（period 只由左侧 label 显示），右侧显示剩余百分比，且 used 空串
// （kimi API 不返回 used 字段）不再渲染出「/100」。
func TestRenderTemplateFmtQuotaPercentOnly(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		"q.used !== ''",
		"return fmtPctNum(q.percent) + '%'",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("模板 fmtQuota 缺少 %q（应：右侧只显示一个百分比 + used 空串降级）", s)
		}
	}
	if strings.Contains(src, "var parts = [q.period]") {
		t.Errorf("fmtQuota 仍拼接 period，应只由左侧 label 显示")
	}
	if strings.Contains(src, "parts.push(q.percent + '%')") {
		t.Errorf("fmtQuota 仍将 percent 作为追加片段，应直接返回 percent 加百分号")
	}
}

// TestRenderTemplateThemeSystem 验证主题三态系统：CSS 变量分组 + 切换按钮 + JS 逻辑。
// 守护 T23 主题系统：未来重构若误删 [data-theme="dark"] 覆盖或 prefers-color-scheme 媒体
// 查询导致深色模式失效，此测试立即失败。
func TestRenderTemplateThemeSystem(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		`html[data-theme="dark"]`,
		`prefers-color-scheme: dark`,
		`:root:not([data-theme="light"])`,
		`--warn-bg`,
		`id="btn-theme"`,
		"THEME_CYCLE",
		"localStorage",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("模板主题系统缺少 %q", s)
		}
	}
	if strings.Contains(src, "background: #fff8e6") {
		t.Errorf("硬编码警告背景色 #fff8e6 仍存在，应使用 var(--warn-bg)")
	}
}

// TestRenderTemplateClassifyAndSortThreeGroups 守护 T32 三组分类（套餐/余额/异常）
// 取代 T22 两桶互斥分流。both 类型双组都加，异常组单独列出。
func TestRenderTemplateClassifyAndSortThreeGroups(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)

	oldMarkers := []string{"function tierPlan", "function tierBalance", "套餐类档1", "余额类对称"}
	for _, m := range oldMarkers {
		if strings.Contains(src, m) {
			t.Errorf("旧四档分类逻辑残留 %q", m)
		}
	}

	newMarkers := []string{
		"var planBuckets = [[], []]",
		"var balanceBuckets = [[], []]",
		"var errorList = []",
		"var inPlan = (t === 'plan')",
		"var inBalance = (t === 'balance')",
		"if (isError) {",
		"error: errorList",
		"if (grouped.error.length > 0) {",
		"<h3>异常</h3>",
		"<h3>套餐用量</h3>",
		"<h3>余额用量</h3>",
	}
	for _, m := range newMarkers {
		if !strings.Contains(src, m) {
			t.Errorf("T32 三组分类逻辑缺少 %q", m)
		}
	}
}

// TestRenderTemplateT33ClassificationFixes 守护 T33 分类修复（T44 更新）：
//   - 未知 provider（skip）完全跳过；类型不匹配自 T44 起不再隐藏，归异常组显示
//   - 智谱 bug：分组以 usage_type 为主，Quotas 仅当 usage_type 为 plan/both 才算套餐数据
//   - provider 下拉含「未知」选项，提交时 prompt 输入自定义名称
func TestRenderTemplateT33ClassificationFixes(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		"if (u.skip) continue;",
		"var inPlan = (t === 'plan') || (t === 'both' && (hasPlan || hasQuotas))",
		"var inBalance = (t === 'balance') || (t === 'both' && hasBalance)",
		`<option value="__unknown__">未知</option>`,
		"window.prompt('输入自定义 provider 名称",
		"if (!provider) { toast('已取消：未输入 provider 名称', 'warn'); return; }",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("T33 修复逻辑缺少 %q", s)
		}
	}
	// 旧判据不应残留：Quotas 不得再单独作为套餐判据
	if strings.Contains(src, "var inPlan = (t === 'plan') || hasPlan || hasQuotas;") {
		t.Errorf("旧 inPlan 判据残留（Quotas 单独作为套餐判据），应改为以 usage_type 为主")
	}
	// T44 回归：类型不匹配不得以任何字符串判据隐藏（无消息文案耦合）。
	for _, banned := range []string{"indexOf('类型不匹配')", "indexOf('该 key 标记为')"} {
		if strings.Contains(src, banned) {
			t.Errorf("T44 后不应存在按 usage.error 文案隐藏卡片的判据 %q", banned)
		}
	}
}

// TestRenderTemplateDisabledKeyType 守护 T45 disabled 类型：
//   - 三处类型下拉（添加/编辑/表格行内）含「禁用」选项
//   - buildUsageEntries 对 disabled key 标 skip（不发起请求、不渲染卡片）
//   - keyTypeLabelChinese 映射「禁用」
func TestRenderTemplateDisabledKeyType(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		`<option value="disabled">禁用</option>`,                                                        // add + edit 两处静态下拉
		`'<option value="disabled"' + (currentKt === 'disabled' ? ' selected' : '') + '>禁用</option>'`, // 表格行内下拉
		"skip: !k.provider_display_name || k.key_type === 'disabled'",
		"if (kt === 'disabled') return '禁用';",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("T45 disabled 类型缺少 %q", s)
		}
	}
}

// TestRenderTemplateCardNamePriority 守护 renderCard 的卡片名称优先级
// （account 优先，其次 provider_display_name，最后 provider 兜底）。
func TestRenderTemplateCardNamePriority(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "var name = u.account || u.provider_display_name || u.provider;") {
		t.Errorf("renderCard 名称优先级被改动，应保持 account 优先")
	}
}

// TestRenderTemplateAccountGroup 守护 T36 账户语义（account 即账户）+ 编辑增强：
//   - 录入表单含 #add-account（账户），编辑 modal 含 #edit-provider/#edit-key(留空保持)/#edit-account
//   - key 管理表「账户」列显示 account（空显示 '-'）
//   - groupByAccount 按 (provider, account, key_type) 聚合（isGroup + provider + ':' + account + ':' + keyType）
//   - classifyAndSort 处理 isGroup 分支、renderGroupCard 存在
func TestRenderTemplateAccountGroup(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		`账户（同账户聚合显示，可选）`,
		`id="edit-provider"`,
		`id="edit-key"`,
		`placeholder="留空保持原 key"`,
		`id="edit-account"`,
		"function groupByAccount",
		"isGroup: true",
		"u.provider + ':' + g + ':' + kt",
		"(u.account || '').trim()",
		"var kt = u.key_type || 'both';",
		"account: g",
		"if (u.isGroup) {",
		"classifyGroup(u, planBuckets, balanceBuckets, errorList)",
		"function renderGroupCard",
		"classifyAndSort(groupByAccount(usage))",
		"<th>账户</th>",
		"escapeText(k.account || '-')",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("T36 账户语义/编辑增强缺少 %q", s)
		}
	}
	// group 字段 UI 不应残留（add-group/edit-group/group 聚合字段）
	oldMarkers := []string{`id="add-group"`, `id="edit-group"`, "u.group", "k.group", "escapeText(u.group)"}
	for _, m := range oldMarkers {
		if strings.Contains(src, m) {
			t.Errorf("T36 group 字段残留 %q", m)
		}
	}
	// key 输入框不得预填原 key：打开 modal 时强制清空
	if !strings.Contains(src, "$('edit-key').value = '';") {
		t.Errorf("编辑 modal 应打开时清空 key 输入框（不显示原 key）")
	}
}

// TestRenderTemplateGroupTypeIsolation 守护同账户不同类型 key 分开展示：
//   - groupByAccount 聚合键含 key_type：同 (provider, account) 但 key_type 不同
//     （一个 plan 一个 balance）分属不同 group，各自独立显示
//   - refreshCard 关联过滤同样按 key_type 隔离，刷新不跨类型联动
func TestRenderTemplateGroupTypeIsolation(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)
	wantSnippets := []string{
		"var kt = u.key_type || 'both';",
		"var key = u.provider + ':' + g + ':' + kt;",
		"key_type: kt,",
		"(e.key_type || 'both') === (target.key_type || 'both')",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("同账户不同类型隔离逻辑缺少 %q", s)
		}
	}
}

// TestRenderTemplateT38AddModal 守护 T38「添加 Key」从内联表单改造为按钮 + 弹窗：
//   - 表格上方只剩 #btn-add 按钮，不再有内联 #add-form（#add-form 现在仅在 #add-modal 内）
//   - #add-modal 复用 modal-backdrop 样式，含提供商/API Key/类型/账户/备注 + 取消/保存
//   - JS 提供 openAddModal/closeAddModal + 必填校验（provider/key 各自独立 toast）
//   - 「未知」provider prompt 逻辑仍保留在 submit 处理中
//   - 提交成功后关闭弹窗（closeAddModal）
func TestRenderTemplateT38AddModal(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)

	wantSnippets := []string{
		`id="btn-add"`,
		`id="add-modal"`,
		`class="modal-backdrop"`,
		"添加 Key", // 弹窗标题
		`<form id="add-form"`,
		`id="add-provider"`,
		`id="add-key"`,
		`id="add-keytype"`,
		`id="add-account"`,
		`id="add-note"`,
		`id="add-cancel"`,
		"function openAddModal",
		"function closeAddModal",
		"on('btn-add', 'click', openAddModal)",
		"on('add-cancel', 'click', closeAddModal)",
		"if (ev.target === $('add-modal')) closeAddModal()",
		"closeAddModal()",
		"请选择提供商",
		"API Key 不能为空",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("T38 添加弹窗缺少 %q", s)
		}
	}

	// 旧合并提示不应残留（拆分为独立的 provider / key 校验）
	oldMarkers := []string{"provider 和 key 不能为空"}
	for _, m := range oldMarkers {
		if strings.Contains(src, m) {
			t.Errorf("T38 旧合并校验提示残留 %q，应拆分为独立 toast", m)
		}
	}

	// 「未知」provider prompt 逻辑必须保留（T33 守护，这里再交叉守护）
	unknownMarkers := []string{
		"provider === '__unknown__'",
		"window.prompt('输入自定义 provider 名称",
	}
	for _, m := range unknownMarkers {
		if !strings.Contains(src, m) {
			t.Errorf("T38 「未知」provider prompt 逻辑缺失 %q", m)
		}
	}

	// HTML 必须出现至少两个 modal-backdrop（edit-modal + add-modal）
	if strings.Count(src, `class="modal-backdrop"`) < 2 {
		t.Errorf("应至少有两个 modal-backdrop（edit + add），实际 %d", strings.Count(src, `class="modal-backdrop"`))
	}
}

// TestRenderTemplateT41UsageNoteFallback 守护 T41「用量说明数据驱动回退」：
//   - 卡片动作区保留「前往用量页」直达与「用量说明」两分支（console_usage_url 有/无）
//   - openUsageInfo 文案优先取后端下发的 usage_note，缺失时用 USAGE_INFO_FALLBACK
//   - info-modal 弹框结构 + info-close / 背景点击关闭监听存在
//   - fetchEntry 透传 usage_note，buildUsageEntries 初始化该字段
func TestRenderTemplateT41UsageNoteFallback(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)

	wantSnippets := []string{
		`data-action="usage-url"`,
		`data-action="usage-info"`,
		`id="info-modal"`,
		`id="info-title"`,
		`id="info-body"`,
		`id="info-close"`,
		"function openUsageInfo",
		"function closeUsageInfo",
		"on('info-close', 'click', closeUsageInfo)",
		"if (ev.target === $('info-modal')) closeUsageInfo()",
		"entry.usage_note || USAGE_INFO_FALLBACK",
		"usage_note = u.usage_note || ''",
		"usage_note: ''",
	}
	for _, s := range wantSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("T41 模板缺少片段 %q", s)
		}
	}
}

func TestRenderTemplateEmpty(t *testing.T) {
	h := newTestHandler(t)
	w := doGET(h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "app-data") {
		t.Errorf("空库渲染应仍注入初始数据脚本块")
	}
}

func TestRenderTemplateContainsInitialData(t *testing.T) {
	h := newTestHandler(t)
	w := doGET(h, "/")
	body := w.Body.String()

	start := strings.Index(body, `<script id="app-data" type="application/json">`)
	end := strings.Index(body, `</script>`)
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("未找到 app-data script 标签")
	}
	rawJSON := body[start+len(`<script id="app-data" type="application/json">`) : end]

	var snap server.PageSnapshot
	if err := json.Unmarshal([]byte(rawJSON), &snap); err != nil {
		t.Fatalf("InitialDataJSON 不是合法 JSON: %v\nraw=%s", err, rawJSON)
	}
	if len(snap.Keys) != 0 {
		t.Errorf("空库 keys = %d, want 0", len(snap.Keys))
	}
	if len(snap.Usage) != 0 {
		t.Errorf("空库 usage = %d, want 0", len(snap.Usage))
	}
	if len(snap.Providers) != 0 {
		t.Errorf("空库 providers = %d, want 0（未注入 mock）", len(snap.Providers))
	}
	if !snap.Features.DashboardEnabled || !snap.Features.KeysEnabled || snap.Features.CardFilter.Mode != "blacklist" || snap.Features.CardFilter.Cards == nil {
		t.Errorf("默认 features 未注入: %+v", snap.Features)
	}
}

func TestTemplateInjectsDisabledFeatures(t *testing.T) {
	cfg := appconf.Default()
	cfg.Dashboard.Enabled = false
	cfg.Dashboard.CardFilter = appconf.CardFilter{Mode: "whitelist", Cards: []string{"stub/account"}}
	cfg.Keys.Enabled = false
	w := doGET(newTestHandler(t, cfg), "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, value := range []string{`"dashboardEnabled":false`, `"keysEnabled":false`, `"mode":"whitelist"`, `"stub/account"`, `id="dashboard-section"`, `id="keys-section"`, `id="all-disabled"`} {
		if !strings.Contains(body, value) {
			t.Errorf("page response missing %q", value)
		}
	}
}

// TestXSSEscapedInInitialData 验证 InitialDataJSON 的 JSON 注入安全：
// 即便用户输入含 < > & 等字符，json.Marshal 默认转义为 \u003c 等，
// 浏览器解析 script 标签时不会提前关闭。
func TestXSSEscapedInInitialData(t *testing.T) {
	h := newTestHandler(t)
	w := doGET(h, "/")
	body := w.Body.String()

	start := strings.Index(body, `<script id="app-data" type="application/json">`)
	end := strings.Index(body, `</script>`)
	if start < 0 || end < 0 {
		t.Fatalf("未找到 app-data 标签")
	}
	rawJSON := body[start+len(`<script id="app-data" type="application/json">`) : end]

	if strings.Contains(rawJSON, "</script>") {
		t.Errorf("JSON 含未转义 </script>，浏览器会提前关闭 script 标签: %s", rawJSON)
	}

	var snap server.PageSnapshot
	if err := json.Unmarshal([]byte(rawJSON), &snap); err != nil {
		t.Fatalf("JSON 解析失败: %v\nraw=%s", err, rawJSON)
	}
	_ = snap
}

// TestXSSEscapedInHTMLStatic 验证模板源里所有 {{...}} 仅出现 InitialDataJSON
// 一个注入点——保证模板不会 inline 拼接用户数据到 HTML。
func TestXSSEscapedInHTMLStatic(t *testing.T) {
	data, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(data)

	if !strings.Contains(src, "{{.InitialDataJSON}}") {
		t.Errorf("模板缺少 InitialDataJSON 注入点")
	}
	count := strings.Count(src, "{{")
	if count > 2 {
		t.Errorf("模板含 %d 个 {{ ，超出预期（应仅 InitialDataJSON 注入点）", count)
	}
}

func TestNotFound(t *testing.T) {
	h := newTestHandler(t)
	w := doGET(h, "/missing")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestSnapshotErrorReturns500(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := server.NewServer(st, cache.New(5*time.Minute), 5*time.Minute)
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	h := NewHandler(s)
	w := doGET(h, "/")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (db 已关)", w.Code)
	}
}
