// Package server 提供 HTTP 服务端（监听 localhost，无认证）。
//
// 本文件实现 REST API handlers：
//
//	GET    /api/keys            全部 key 列表（key 一律脱敏）
//	POST   /api/keys            添加 key（校验 provider 存在 + 前缀匹配）
//	PUT    /api/keys/reorder    整体重排 key 顺序（body: {"ids":[...]}）
//	DELETE /api/keys/{id}       删除 key
//	PATCH  /api/keys/{id}       更新 account/note
//	GET    /api/keys/{id}/usage 单 key 用量（走缓存层）
//	GET    /api/usage           全部 key 用量（并发拉取，局部失败 200）
//	POST   /api/refresh         清缓存后强制刷新全部用量
//
// 路由使用 Go 1.22+ ServeMux pattern，不引入第三方路由框架。
// Origin 校验中间件（写操作 CSRF 防护）在 middleware.go（任务 15）实现，
// 本文件只注册路由，Handler() 返回的 mux 供中间件包裹。
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-usage/internal/appconf"
	"ai-usage/internal/cache"
	"ai-usage/internal/provider"
	"ai-usage/internal/store"
)

// modelscopeTTL 是 modelscope 查询的强制缓存时长。
// 每次查询消耗 1 次请求额度（learnings T12），必须用 1h 缓存避免浪费。
const modelscopeTTL = time.Hour

// Server 持有 REST API 的依赖。
//
// Lookup 抽象 provider 查找（默认按 id 再按 alias 查全局注册表），
// 是导出字段以便 internal/web 等跨包测试直接替换。
type Server struct {
	store    *store.Store
	cache    *cache.Cache
	cacheTTL time.Duration
	appConf  appconf.Config
	Lookup   func(id string) (provider.Provider, bool)
	mux      *http.ServeMux
}

// NewServer 构造 server 实例。
//
// cacheTTL 是普通 provider 的用量缓存时长（对应 config.CacheTTL）；
// modelscope 固定使用 1h 强制缓存，见 forceTTL。
func NewServer(st *store.Store, c *cache.Cache, cacheTTL time.Duration, appConfigs ...appconf.Config) *Server {
	appConfig := appconf.Default()
	if len(appConfigs) > 0 {
		appConfig = appConfigs[0]
	}
	s := &Server{
		store:    st,
		cache:    c,
		cacheTTL: cacheTTL,
		appConf:  appConfig,
		Lookup: func(id string) (provider.Provider, bool) {
			if p, ok := provider.Get(id); ok {
				return p, true
			}
			return provider.GetByAlias(id)
		},
	}
	s.mux = s.routes()
	return s
}

// Handler 返回注册好路由的 http.Handler。
// 任务 15 的中间件在此包裹：middleware.Wrap(s.Handler())。
func (s *Server) Handler() http.Handler { return s.mux }

// routes 注册全部 REST 端点（Go 1.22+ method+path pattern）。
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/keys", s.handleListKeys)
	mux.HandleFunc("POST /api/keys", s.requireKeys(s.handleAddKey))
	mux.HandleFunc("PUT /api/keys/reorder", s.requireKeys(s.handleReorderKeys))
	mux.HandleFunc("DELETE /api/keys/{id}", s.requireKeys(s.handleDeleteKey))
	mux.HandleFunc("PATCH /api/keys/{id}", s.requireKeys(s.handlePatchKey))
	mux.HandleFunc("GET /api/keys/{id}/key", s.handleGetKey)
	mux.HandleFunc("GET /api/keys/{id}/usage", s.requireDashboard(s.handleKeyUsage))
	mux.HandleFunc("POST /api/keys/{id}/usage", s.requireDashboard(s.handleRefreshKeyUsage))
	mux.HandleFunc("GET /api/usage", s.requireDashboard(s.handleAllUsage))
	mux.HandleFunc("POST /api/refresh", s.requireDashboard(s.handleRefresh))
	mux.HandleFunc("POST /api/credentials", s.requireKeys(s.handleSetCredential))
	mux.HandleFunc("GET /api/credentials", s.handleGetCredential)
	mux.HandleFunc("DELETE /api/credentials", s.requireKeys(s.handleDeleteCredential))
	return mux
}

func (s *Server) requireDashboard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.appConf.Dashboard.Enabled {
			writeError(w, http.StatusForbidden, "用量仪表盘功能已在配置文件中禁用")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireKeys(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.appConf.Keys.Enabled {
			writeError(w, http.StatusForbidden, "Key 管理功能已在配置文件中禁用")
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 响应模型
// ---------------------------------------------------------------------------

// keyResponse 是 key 的 API 表示。key 字段一律脱敏，绝不返回明文。
type keyResponse struct {
	ID                  int64            `json:"id"`
	Provider            string           `json:"provider"`
	ProviderDisplayName string           `json:"provider_display_name"`
	KeyMasked           string           `json:"key_masked"`
	KeyType             string           `json:"key_type"`
	Account             string           `json:"account,omitempty"`
	Note                string           `json:"note,omitempty"`
	CreatedAt           string           `json:"created_at"`
	ConsoleURL          string           `json:"console_url"`
	PlanMeta            planMetaResponse `json:"plan_meta"`
}

// planMetaResponse 是 PlanMeta 的 JSON 表示（provider.PlanMeta 无 json tag，需自定义）。
type planMetaResponse struct {
	PlanName  string `json:"plan_name"`
	PlanPrice string `json:"plan_price"`
	ApiPrice  string `json:"api_price"`
}

// singleUsageResponse 是 GET/POST /api/keys/{id}/usage 的响应。
// 各分支字段不同：成功带 usage；类型不匹配带 usage.error="类型不匹配…"；
// 其他 fetch 错误带顶层 error。一律 HTTP 200（局部失败原则）。
type singleUsageResponse struct {
	KeyID           int64           `json:"key_id"`
	Provider        string          `json:"provider"`
	Account         string          `json:"account,omitempty"`
	UsageType       string          `json:"usage_type"`
	Usage           *provider.Usage `json:"usage,omitempty"`
	Error           string          `json:"error,omitempty"`
	ConsoleURL      string          `json:"console_url"`
	ConsoleUsageURL string          `json:"console_usage_url,omitempty"`
	UsageNote       string          `json:"usage_note,omitempty"`
	UpdatedAt       *time.Time      `json:"updated_at,omitempty"`
	Credential      *CredentialMeta `json:"credential,omitempty"`
}

// usageResult 是 GET /api/usage 结果数组中的单 key 条目。
// 单 key 失败 → error 字段承载，整体仍 200。
// Skip=true 表示该 key 不参与查询与显示（未知 provider 或禁用 key），前端应完全跳过不显示。
type usageResult struct {
	KeyID               int64           `json:"key_id"`
	Provider            string          `json:"provider"`
	ProviderDisplayName string          `json:"provider_display_name"`
	Account             string          `json:"account,omitempty"`
	UsageType           string          `json:"usage_type"`
	Usage               *provider.Usage `json:"usage,omitempty"`
	Error               string          `json:"error,omitempty"`
	ConsoleURL          string          `json:"console_url"`
	ConsoleUsageURL     string          `json:"console_usage_url,omitempty"`
	UsageNote           string          `json:"usage_note,omitempty"`
	Skip                bool            `json:"skip,omitempty"`
	Credential          *CredentialMeta `json:"credential,omitempty"`
}

// allUsageResponse 是 GET /api/usage 与 POST /api/refresh 的响应。
type allUsageResponse struct {
	Results   []usageResult `json:"results"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// CredentialLink 与 CredentialMeta 是凭证录入说明的 API 表示。
type CredentialLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type CredentialMeta struct {
	Kind        string           `json:"kind"`
	Label       string           `json:"label"`
	Help        string           `json:"help"`
	Placeholder string           `json:"placeholder"`
	Links       []CredentialLink `json:"links"`
}

type credentialRequest struct {
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	Credential string `json:"credential"`
}

type credentialResponse struct {
	Provider      string `json:"provider"`
	Account       string `json:"account"`
	HasCredential bool   `json:"hasCredential"`
	Masked        string `json:"masked"`
	UpdatedAt     string `json:"updatedAt"`
}

func (s *Server) handleSetCredential(w http.ResponseWriter, r *http.Request) {
	var req credentialRequest
	if err := decodeJSON(r, &req); err != nil || req.Provider == "" || req.Credential == "" {
		writeError(w, http.StatusBadRequest, "请求参数无效")
		return
	}
	if err := s.store.SetAccountCredential(req.Provider, req.Account, req.Credential); err != nil {
		writeError(w, http.StatusInternalServerError, "保存凭证失败")
		return
	}
	s.invalidateAccountUsage(req.Provider, req.Account)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	providerName, account := r.URL.Query().Get("provider"), r.URL.Query().Get("account")
	if providerName == "" {
		writeError(w, http.StatusBadRequest, "provider 不能为空")
		return
	}
	resp := credentialResponse{Provider: providerName, Account: account}
	credential, err := s.store.GetAccountCredential(providerName, account)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "读取凭证失败")
		return
	}
	if credential != nil {
		resp.HasCredential = true
		resp.UpdatedAt = credential.UpdatedAt
		resp.Masked = maskCookie(credential.Credential)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	providerName, account := r.URL.Query().Get("provider"), r.URL.Query().Get("account")
	if providerName == "" {
		writeError(w, http.StatusBadRequest, "provider 不能为空")
		return
	}
	if err := s.store.DeleteAccountCredential(providerName, account); err != nil {
		writeError(w, http.StatusInternalServerError, "删除凭证失败")
		return
	}
	s.invalidateAccountUsage(providerName, account)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func credentialMeta(p provider.Provider) *CredentialMeta {
	specProvider, ok := p.(interface {
		CredentialSpec() *provider.CredentialSpec
	})
	if !ok {
		return nil
	}
	spec := specProvider.CredentialSpec()
	if spec == nil {
		return nil
	}
	links := make([]CredentialLink, 0, len(spec.Links))
	for _, link := range spec.Links {
		links = append(links, CredentialLink{Label: link.Label, URL: link.URL})
	}
	return &CredentialMeta{Kind: spec.Kind, Label: spec.Label, Help: spec.Help, Placeholder: spec.Placeholder, Links: links}
}

func maskCookie(cookie string) string {
	if len(cookie) <= 4 {
		return strings.Repeat("*", len(cookie))
	}
	return cookie[:2] + strings.Repeat("*", len(cookie)-4) + cookie[len(cookie)-2:]
}

func (s *Server) invalidateAccountUsage(providerName, account string) {
	records, err := s.store.ListKeys()
	if err != nil {
		s.cache.Clear()
		return
	}
	for _, rec := range records {
		if rec.Provider == providerName && rec.Account == account {
			s.cache.Invalidate(s.usageCacheKey(rec))
		}
	}
}

// ---------------------------------------------------------------------------
// key handlers
// ---------------------------------------------------------------------------

// handleListKeys GET /api/keys：返回全部 key 列表（脱敏）。
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.ListKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 key 列表失败")
		return
	}
	resp := make([]keyResponse, 0, len(records))
	for _, rec := range records {
		resp = append(resp, s.toKeyResponse(rec))
	}
	writeJSON(w, http.StatusOK, resp)
}

// addKeyRequest 是 POST /api/keys 的请求体。
type addKeyRequest struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
	KeyType  string `json:"key_type"`
	Account  string `json:"account"`
	Note     string `json:"note"`
}

// normalizeKeyType 归一化 key_type：空/非法 → "both"（默认不区分）。
func normalizeKeyType(kt string) string {
	switch kt {
	case store.KeyTypePlan, store.KeyTypeBalance, store.KeyTypeBoth, store.KeyTypeDisabled:
		return kt
	default:
		return store.KeyTypeBoth
	}
}

// keyMatchesProvider 校验 key 是否符合 provider 的格式约束。
// 有 KeyPattern（正则完整匹配）优先用正则；否则退化为 KeyPrefixes 前缀匹配；
// 两者都未声明（nil/空）视为不校验，恒通过（fallback 场景）。
func keyMatchesProvider(p provider.Provider, key string) bool {
	if pattern := p.KeyPattern(); pattern != "" {
		matched, err := regexp.MatchString(pattern, key)
		return err == nil && matched
	}
	for _, prefix := range p.KeyPrefixes() {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return len(p.KeyPrefixes()) == 0
}

// handleAddKey POST /api/keys：添加 key。
//
// 校验顺序：provider 非空 → key 非空 → provider 已注册时做前缀匹配
// （KeyPrefixes 若声明）→ 插入。未知 provider（不在注册表）仍允许添加：
// provider 名以输入值原样存储（仅出现在 key 管理，用量查询返回 skip），跳过前缀校验。
// 重复（store 唯一约束）→ 409。
func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var req addKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	providerName := strings.TrimSpace(req.Provider)
	if providerName == "" {
		writeError(w, http.StatusBadRequest, "provider 不能为空")
		return
	}

	if strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, "key 不能为空")
		return
	}

	// 未知 provider fallback：不拦截，直接用输入名存储，跳过前缀校验。
	if p, ok := s.Lookup(providerName); ok && !keyMatchesProvider(p, req.Key) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("key 格式不匹配 %s 的格式", p.ID()))
		return
	}

	inserted, err := s.store.AddKey(providerName, req.Key, normalizeKeyType(req.KeyType), req.Account, req.Note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "写入数据库失败")
		return
	}
	if !inserted {
		writeError(w, http.StatusConflict, "该 key 已存在")
		return
	}

	// AddKey 不返回新记录 id，回查定位后返回完整条目。
	rec, err := s.findRecord(providerName, req.Key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "添加成功但读取记录失败")
		return
	}
	writeJSON(w, http.StatusCreated, s.toKeyResponse(*rec))
}

// reorderRequest 是 PUT /api/keys/reorder 的请求体：完整的 key id 顺序数组。
type reorderRequest struct {
	IDs []int64 `json:"ids"`
}

// handleReorderKeys PUT /api/keys/reorder：按前端拖拽结果整体重排 key 顺序。
//
// 校验：ids 非空、无重复，且与现有 key 的 id 集合完全一致（防止拖拽期间
// 其他并发变更导致部分重排破坏顺序语义）；不匹配 → 400。成功 → 204。
func (s *Server) handleReorderKeys(w http.ResponseWriter, r *http.Request) {
	var req reorderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids 不能为空")
		return
	}

	records, err := s.store.ListKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 key 列表失败")
		return
	}

	// id 集合校验：现有 key 必须恰好等于请求中的 id 集合（无缺失、无多余、无重复）。
	existing := make(map[int64]struct{}, len(records))
	for i := range records {
		existing[records[i].ID] = struct{}{}
	}
	seen := make(map[int64]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if _, dup := seen[id]; dup {
			writeError(w, http.StatusBadRequest, "ids 包含重复 id")
			return
		}
		seen[id] = struct{}{}
		if _, ok := existing[id]; !ok {
			writeError(w, http.StatusBadRequest, "ids 包含不存在的 key id")
			return
		}
	}
	if len(seen) != len(existing) {
		writeError(w, http.StatusBadRequest, "ids 必须包含全部 key，不能遗漏")
		return
	}

	if err := s.store.ReorderKeys(req.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, "保存排序失败")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteKey DELETE /api/keys/{id}：删除 key，成功 204。
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的 key id")
		return
	}

	rec, err := s.store.GetKey(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "key 不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取数据库失败")
		return
	}

	if err := s.store.DeleteKey(id); err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}

	// 顺带清除该 key 的用量缓存，避免删除后残留。
	s.cache.Invalidate(s.usageCacheKey(*rec))
	w.WriteHeader(http.StatusNoContent)
}

// fullKeyResponse 是 GET /api/keys/{id}/key 的响应体。
// 返回完整明文 key 给前端复制按钮使用——这是对 GET 不返回完整 key 的
// guardrail 的有意识例外：仅用户主动点击触发，需 Basic Auth（T21）/同源保护。
type fullKeyResponse struct {
	Key string `json:"key"`
}

// handleGetKey GET /api/keys/{id}/key：返回完整 key 明文。
//
// 安全模型：此端点是用户主动触发（前端「复制」按钮），
// 默认 GET 列表的脱敏规则不适用于此端点；仍然受认证（T21 Wrap）和
// 同源（OriginCheck 仅拦写方法）的间接保护——
// 浏览器只能经同源前端 fetch，无法直接跨站访问。
//
// 日志路径不记录 key：路径只到 /api/keys/{id}/key，不带查询参数。
func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的 key id")
		return
	}

	rec, err := s.store.GetKey(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "key 不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取数据库失败")
		return
	}

	writeJSON(w, http.StatusOK, fullKeyResponse{Key: rec.Key})
}

// patchKeyRequest 是 PATCH /api/keys/{id} 的请求体。
// 指针字段区分「未提供」与「显式置空」：未提供的字段保留原值（部分更新语义）。
// key 特殊：指针非 nil 且值为空串/纯空格 → 保持原 key（用户「不填 apikey 保持原 key」）。
type patchKeyRequest struct {
	Provider *string `json:"provider"`
	Key      *string `json:"key"`
	KeyType  *string `json:"key_type"`
	Account  *string `json:"account"`
	Note     *string `json:"note"`
}

// handlePatchKey PATCH /api/keys/{id}：更新 provider/key/keyType/account/note，成功 200。
//
// 指针字段部分更新语义：未提供（nil）→ 保留原值。key 特殊：提供但值为
// 空串/纯空格 → 保留原 key（「不填 apikey 保持原 key」）；provider 提供但为空 → 400。
// provider 变更时：已知 provider 校验 key 前缀匹配（KeyPrefixes），未知 provider
// fallback 存输入值（与 handleAddKey 一致）；新 (provider, key) 与其他记录冲突 → 409。
func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的 key id")
		return
	}

	cur, err := s.store.GetKey(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "key 不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取数据库失败")
		return
	}

	// 空 body（io.EOF）视为不更新任何字段；其他解析错误 → 400。
	var req patchKeyRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	providerName, key := cur.Provider, cur.Key
	account, note, keyType := cur.Account, cur.Note, cur.KeyType
	if req.Provider != nil {
		p := strings.TrimSpace(*req.Provider)
		if p == "" {
			writeError(w, http.StatusBadRequest, "provider 不能为空")
			return
		}
		providerName = p
	}
	if req.Key != nil {
		if k := strings.TrimSpace(*req.Key); k != "" {
			key = k
		}
	}
	if req.KeyType != nil {
		keyType = normalizeKeyType(*req.KeyType)
	}
	if req.Account != nil {
		account = *req.Account
	}
	if req.Note != nil {
		note = *req.Note
	}

	// provider 变更：已知 provider 校验 key 格式；未知 provider 不拦截（存输入值）。
	if providerName != cur.Provider {
		if p, ok := s.Lookup(providerName); ok && !keyMatchesProvider(p, key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("key 格式不匹配 %s 的格式", p.ID()))
			return
		}
	}

	// 冲突检查：新 (provider, key) 组合若与其他记录重复（UNIQUE 约束）→ 409，自己除外。
	if providerName != cur.Provider || key != cur.Key {
		records, err := s.store.ListKeys()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取数据库失败")
			return
		}
		for i := range records {
			if records[i].ID == cur.ID {
				continue
			}
			if records[i].Provider == providerName && records[i].Key == key {
				writeError(w, http.StatusConflict, "该 key 已存在")
				return
			}
		}
	}

	if err := s.store.UpdateKeyFull(cur.ID, providerName, key, keyType, account, note); err != nil {
		writeError(w, http.StatusInternalServerError, "更新失败")
		return
	}

	// provider/key 变更后旧缓存键失效；键前缀随 provider 变化，新旧都清。
	s.cache.Invalidate(s.usageCacheKey(*cur))
	cur.Provider, cur.Key, cur.KeyType, cur.Account, cur.Note = providerName, key, keyType, account, note
	s.cache.Invalidate(s.usageCacheKey(*cur))

	writeJSON(w, http.StatusOK, s.toKeyResponse(*cur))
}

// ---------------------------------------------------------------------------
// usage handlers
// ---------------------------------------------------------------------------

// handleKeyUsage GET /api/keys/{id}/usage：单 key 用量（走缓存层）。
//
// 按 key 的类型标记（plan/balance/both）经 fetchResolved 解析查询；
// 类型不匹配 → 200 + usage.error="类型不匹配…"；其他 fetch 错误 → 200 + error 字段
// （HTTP 200 局部失败原则）。
func (s *Server) handleKeyUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的 key id")
		return
	}

	rec, err := s.store.GetKey(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "key 不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取数据库失败")
		return
	}

	writeJSON(w, http.StatusOK, s.fetchSingleUsage(r.Context(), *rec, false))
}

// handleRefreshKeyUsage POST /api/keys/{id}/usage：单 key 强制刷新。
//
// 清除该 key 的缓存后重新拉取（不走已缓存结果），并把新结果写入同账户
// （provider+account+keyType 相同）兄弟 key 的缓存槽位，保证下次批量查询
// 同账户数据一致且不重复打上游。响应结构与 GET 单 key 用量一致。
// 前端「刷新全部」按卡片并行调用本端点，单个 provider 卡住不影响其他卡片。
func (s *Server) handleRefreshKeyUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的 key id")
		return
	}

	rec, err := s.store.GetKey(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "key 不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取数据库失败")
		return
	}

	s.cache.Invalidate(s.usageCacheKey(*rec))
	resp := s.fetchSingleUsage(r.Context(), *rec, true)
	writeJSON(w, http.StatusOK, resp)
}

// fetchSingleUsage 拉取单 key 用量并组装 singleUsageResponse。
// force=true（刷新场景）时把结果同步写入同账户兄弟 key 的缓存。
func (s *Server) fetchSingleUsage(ctx context.Context, rec store.KeyRecord, force bool) singleUsageResponse {
	resp := singleUsageResponse{KeyID: rec.ID, Provider: rec.Provider, Account: rec.Account}

	// 禁用 key：不发上游查询；该端点前端不会调用（skip 单元被过滤），
	// 仅供 curl 直访时拿到明确说明（HTTP 200 局部失败原则）。
	if normalizeKeyType(rec.KeyType) == store.KeyTypeDisabled {
		resp.UsageType = store.KeyTypeDisabled
		resp.Error = "该 key 已禁用，不参与用量查询"
		return resp
	}

	p, ok := s.Lookup(rec.Provider)
	if !ok {
		resp.UsageType = provider.UsageTypePlan
		resp.Error = "provider 未注册"
		return resp
	}
	resp.ConsoleURL = p.ConsoleURL()
	resp.ConsoleUsageURL = p.UsageConsoleURL(normalizeKeyType(rec.KeyType))
	resp.UsageNote = p.UsageNote()
	resp.Credential = credentialMeta(p)

	usage, effectiveType, err := s.cache.GetOrFetchTyped(ctx, s.usageCacheKey(rec), s.forceTTL(p),
		func(ctx context.Context) (*provider.Usage, string, error) {
			return s.fetchResolved(ctx, p, rec)
		})
	resp.UsageType = effectiveType
	if err != nil {
		resp.UsageType = normalizeKeyType(rec.KeyType)
		resp.Error = usageErrorMessage(p, normalizeKeyType(rec.KeyType), err)
		return resp
	}

	resp.Usage = usage
	if force {
		s.syncSiblingCache(rec, p, usage, effectiveType)
	}
	return resp
}

// syncSiblingCache 把代表 key 的刷新结果写入同账户兄弟 key 的缓存槽位。
//
// 兄弟判定：provider 相同 + account 相同（非空）+ keyType 相同。account 为空的
// key 独立账户，不参与同步。列表读取失败静默跳过（刷新结果本身已返回给
// 调用方，兄弟缓存同步只是优化，不阻断主流程）。
func (s *Server) syncSiblingCache(rep store.KeyRecord, p provider.Provider, usage *provider.Usage, typ string) {
	if rep.Account == "" {
		return
	}
	records, err := s.store.ListKeys()
	if err != nil {
		return
	}
	ttl := s.forceTTL(p)
	for i := range records {
		r := records[i]
		if r.ID == rep.ID || r.Provider != rep.Provider || r.Account != rep.Account || r.KeyType != rep.KeyType {
			continue
		}
		s.cache.Put(s.usageCacheKey(r), usage, typ, ttl)
	}
}

// handleAllUsage GET /api/usage：全部 key 用量并发拉取。
// 单 key 失败 → 该条目 error 字段，整体仍 200。
func (s *Server) handleAllUsage(w http.ResponseWriter, r *http.Request) {
	results, err := s.fetchAllUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 key 列表失败")
		return
	}
	writeJSON(w, http.StatusOK, allUsageResponse{Results: results, UpdatedAt: time.Now()})
}

// handleRefresh POST /api/refresh：清除缓存后强制重新拉取全部用量。
// 响应结构与 GET /api/usage 一致。
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.cache.Clear()
	results, err := s.fetchAllUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 key 列表失败")
		return
	}
	writeJSON(w, http.StatusOK, allUsageResponse{Results: results, UpdatedAt: time.Now()})
}

// ---------------------------------------------------------------------------
// 内部辅助
// ---------------------------------------------------------------------------

// toKeyResponse 把存储记录转换为 API 响应，key 用 MaskKey 脱敏。
// console_url/plan_meta/provider_display_name 从注册表查 provider 获取；
// provider 不在注册表时这些字段留空。
func (s *Server) toKeyResponse(rec store.KeyRecord) keyResponse {
	resp := keyResponse{
		ID:        rec.ID,
		Provider:  rec.Provider,
		KeyMasked: provider.MaskKey(rec.Key),
		KeyType:   rec.KeyType,
		Account:   rec.Account,
		Note:      rec.Note,
		CreatedAt: rec.CreatedAt,
	}
	if p, ok := s.Lookup(rec.Provider); ok {
		resp.ProviderDisplayName = p.DisplayName()
		resp.ConsoleURL = p.ConsoleURL()
		meta := p.PlanMeta()
		resp.PlanMeta = planMetaResponse{
			PlanName:  meta.PlanName,
			PlanPrice: meta.PlanPrice,
			ApiPrice:  meta.ApiPrice,
		}
	}
	return resp
}

// findRecord 在全部记录中定位 provider+key 匹配的记录。
// store.AddKey 不返回新记录 id，添加成功后用明文 key 回查（内部比较，不落响应）。
func (s *Server) findRecord(p string, key string) (*store.KeyRecord, error) {
	records, err := s.store.ListKeys()
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].Provider == p && records[i].Key == key {
			return &records[i], nil
		}
	}
	return nil, fmt.Errorf("添加后未找到记录 provider=%q", p)
}

// fetchAllUsage 并发拉取全部 key 的用量。
//
// T36 聚合优化 + T37 收紧：account 非空时按 (provider, account, keyType) 分组——
// 同 provider 同账户(account)同类型(keyType) 的 key 共用同一余额/套餐，只 fetch 一次
// （用组内第一个 key 的记录），结果（usage/usage_type/error/skip）复制给组内所有 key，
// key_id 保留各自值；同账户不同类型（如一个 plan 一个 balance）分属不同组，各自 fetch。
// account 为空的 key 独立请求。组内 key 保证同 key_type（分组条件含 keyType），
// 以组内第一个 key 的类型 fetch 是安全的。单 key fetch 失败只进该条目 error 字段；
// 未知 provider 条目标记 Skip（不查用量）。单 key 端点 /api/keys/{id}/usage 不聚合。
func (s *Server) fetchAllUsage(ctx context.Context) ([]usageResult, error) {
	records, err := s.store.ListKeys()
	if err != nil {
		return nil, err
	}

	// 分组：同 provider 同 account（非空）同 keyType 共享一次 fetch；account 空 → 独立组（用下标做唯一标记）。
	type groupKey struct{ provider, account, keyType string }
	groups := make(map[groupKey][]int) // 组 key → records 下标
	for i := range records {
		gk := groupKey{records[i].Provider, records[i].Account, records[i].KeyType}
		if gk.account == "" {
			gk.account = fmt.Sprintf("\x00unique:%d", i)
		}
		groups[gk] = append(groups[gk], i)
	}

	results := make([]usageResult, len(records))
	var wg sync.WaitGroup
	for _, idxs := range groups {
		wg.Add(1)
		go func(idxs []int) {
			defer wg.Done()
			// 组内第一个 key 代表 fetch（同账户同类型，结果共享）。
			res := s.fetchOneUsage(ctx, records[idxs[0]])
			for _, i := range idxs {
				r := res
				r.KeyID = records[i].ID
				results[i] = r
			}
		}(idxs)
	}
	wg.Wait()
	return results, nil
}

// fetchOneUsage 拉取单个 key 的用量并组装条目。
// results 由调用方按 index 写入（每 goroutine 只写自己组的槽位），无数据竞争。
func (s *Server) fetchOneUsage(ctx context.Context, rec store.KeyRecord) usageResult {
	res := usageResult{KeyID: rec.ID, Provider: rec.Provider, Account: rec.Account}

	// 禁用 key：不查询、不占用缓存，skip=true 与未知 provider 同语义
	// （前端完全跳过：不发起请求、不渲染卡片）。
	if normalizeKeyType(rec.KeyType) == store.KeyTypeDisabled {
		res.Skip = true
		return res
	}

	p, ok := s.Lookup(rec.Provider)
	if !ok {
		// 未知 provider（如「未知」自定义名）：仅存储不查用量，skip 让前端完全跳过。
		res.Skip = true
		return res
	}
	res.ProviderDisplayName = p.DisplayName()
	res.ConsoleURL = p.ConsoleURL()
	res.ConsoleUsageURL = p.UsageConsoleURL(normalizeKeyType(rec.KeyType))
	res.UsageNote = p.UsageNote()
	res.Credential = credentialMeta(p)

	usage, effectiveType, err := s.cache.GetOrFetchTyped(ctx, s.usageCacheKey(rec), s.forceTTL(p),
		func(ctx context.Context) (*provider.Usage, string, error) {
			return s.fetchResolved(ctx, p, rec)
		})
	res.UsageType = effectiveType
	if err != nil {
		res.UsageType = normalizeKeyType(rec.KeyType)
		res.Error = usageErrorMessage(p, normalizeKeyType(rec.KeyType), err)
		return res
	}

	res.Usage = usage
	return res
}

// fetchResolved 按 key 的类型标记解析用量查询。
//
//   - keyType=plan/balance：只查对应类型；adapter 返回 ErrNotSupported → 按 kt 报
//     「不支持X查询」（err==nil，HTTP 200，前端异常组显示，T44）；其他错误原样返回。
//   - keyType=both：plan+balance 都尝试，合并成功结果；两者都无实现 → 报两通道均不支持；
//     一个成功一个失败 → 返回成功结果（失败信息进 Usage.Error）。
//
// 返回的 effectiveType 是解析后的实际类型（"plan"/"balance"/"both"），
// 用于 usageResult.UsageType / singleUsageResponse.UsageType 字段。
func (s *Server) fetchResolved(ctx context.Context, p provider.Provider, rec store.KeyRecord) (out *provider.Usage, effKT string, err error) {
	// 出口统一把剩余百分比收敛为最多两位小数，消除浮点格式化长串。
	defer func() { normalizeQuotaPercents(out) }()
	kt := normalizeKeyType(rec.KeyType)
	credential, missing, err := s.credentialFor(p, rec)
	if err != nil {
		return nil, kt, err
	}
	if missing {
		return missingCredentialUsage(p), kt, nil
	}
	switch kt {
	case store.KeyTypePlan, store.KeyTypeBalance:
		u, err := p.FetchUsage(ctx, credential, kt)
		if err != nil {
			if provider.IsNotSupported(err) {
				// T44 实际情况报错：kt 通道无 fetch 实现即报「不支持 X 查询」。
				// 「仅存储平台」与「能力错位」是同一事实（fetch 未实现 kt），
				// 不做分叉；UsageType 是展示归类标签而非能力声明，不得进报错文案。
				// 前端按 usage.error 归异常组显示（T44 起类型不匹配不再隐藏）。
				return &provider.Usage{Error: notSupportedMsg(kt), UpdatedAt: time.Now()}, kt, nil
			}
			return nil, kt, err
		}
		if u == nil {
			u = &provider.Usage{UpdatedAt: time.Now()}
		}
		sortQuotas(u)
		return u, kt, nil
	default: // both
		return s.fetchBoth(ctx, p, credential, rec)
	}
}

func (s *Server) credentialFor(p provider.Provider, rec store.KeyRecord) (string, bool, error) {
	specProvider, ok := p.(interface {
		CredentialSpec() *provider.CredentialSpec
	})
	if !ok {
		return rec.Key, false, nil
	}
	spec := specProvider.CredentialSpec()
	if spec == nil {
		return rec.Key, false, nil
	}
	credential, err := s.store.GetAccountCredential(rec.Provider, rec.Account)
	if errors.Is(err, sql.ErrNoRows) {
		if spec.KeyFallback {
			return rec.Key, strings.TrimSpace(rec.Key) == "", nil
		}
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(credential.Credential) == "" && spec.KeyFallback {
		return rec.Key, strings.TrimSpace(rec.Key) == "", nil
	}
	return credential.Credential, strings.TrimSpace(credential.Credential) == "", nil
}

func missingCredentialUsage(p provider.Provider) *provider.Usage {
	message := "未配置 " + p.DisplayName() + " 凭证，请点击卡片上的「更新凭证」按钮粘贴"
	switch p.ID() {
	case "mimo":
		message = "未配置 MiMo Cookies，请点击「更新 Cookies」按钮粘贴"
	case "openai":
		message = "未配置 OpenAI Token，请点击「更新 Token」按钮粘贴"
	}
	return &provider.Usage{ErrorCode: "credential_missing", Error: message, UpdatedAt: time.Now()}
}

// notSupportedMsg 按请求的 key 类型生成「不支持」报错（T44 实际情况报错）。
func notSupportedMsg(kt string) string {
	label := kt
	switch kt {
	case store.KeyTypeBalance:
		label = "余额"
	case store.KeyTypePlan:
		label = "套餐"
	}
	return fmt.Sprintf("类型不匹配：此提供商不支持%s查询", label)
}

// fetchBoth 处理 keyType=both：余额 + 套餐两个接口都尝试，合并成功结果。
func (s *Server) fetchBoth(ctx context.Context, p provider.Provider, credential string, rec store.KeyRecord) (*provider.Usage, string, error) {
	balUsage, balErr := p.FetchUsage(ctx, credential, provider.UsageTypeBalance)
	planUsage, planErr := p.FetchUsage(ctx, credential, provider.UsageTypePlan)
	if balErr == nil && balUsage != nil && balUsage.ErrorCode != "" {
		return balUsage, provider.UsageTypeBalance, nil
	}
	if planErr == nil && planUsage != nil && planUsage.ErrorCode != "" {
		return planUsage, provider.UsageTypePlan, nil
	}

	balNotSupported := balErr != nil && provider.IsNotSupported(balErr)
	planNotSupported := planErr != nil && provider.IsNotSupported(planErr)
	balFailed := balErr != nil && !balNotSupported
	planFailed := planErr != nil && !planNotSupported

	if balErr == nil && planErr == nil {
		return mergeUsage(balUsage, planUsage), store.KeyTypeBoth, nil
	}

	if balErr == nil {
		u := balUsage
		if u == nil {
			u = &provider.Usage{UpdatedAt: time.Now()}
		}
		if planFailed {
			u.Error = joinError(u.Error, "套餐查询失败："+usageErrorMessage(p, provider.UsageTypePlan, planErr))
		}
		return u, provider.UsageTypeBalance, nil
	}

	if planErr == nil {
		u := planUsage
		if u == nil {
			u = &provider.Usage{UpdatedAt: time.Now()}
		}
		if balFailed {
			u.Error = joinError(u.Error, "余额查询失败："+usageErrorMessage(p, provider.UsageTypeBalance, balErr))
		}
		return u, provider.UsageTypePlan, nil
	}

	if balNotSupported && planNotSupported {
		return &provider.Usage{
			Error:     "类型不匹配：此提供商不支持余额和套餐查询",
			UpdatedAt: time.Now(),
		}, store.KeyTypeBoth, nil
	}

	var msgs []string
	if balFailed {
		msgs = append(msgs, "余额查询失败："+usageErrorMessage(p, provider.UsageTypeBalance, balErr))
	}
	if planFailed {
		msgs = append(msgs, "套餐查询失败："+usageErrorMessage(p, provider.UsageTypePlan, planErr))
	}
	return &provider.Usage{
		Error:     strings.Join(msgs, "；"),
		UpdatedAt: time.Now(),
	}, store.KeyTypeBoth, nil
}

func usageErrorMessage(p provider.Provider, keyType string, err error) string {
	status := httpStatusFromError(err)
	if p.ID() == "opencode-go" && status == http.StatusForbidden {
		return "OpenCode Go 拒绝访问（HTTP 403），请检查套餐或权限条件"
	}
	if provider.IsAuthError(err) {
		statusMessage := ""
		if status != 0 {
			statusMessage = fmt.Sprintf("（HTTP %d）", status)
		}
		switch p.ID() {
		case "deepseek":
			return fmt.Sprintf("DeepSeek 认证失败%s，请检查 Key 是否有效", statusMessage)
		case "minimax":
			return fmt.Sprintf("MiniMax 认证失败%s，请检查 Key 是否有效", statusMessage)
		case "openai":
			return fmt.Sprintf("OpenAI 认证失败%s，请检查 Token 是否有效", statusMessage)
		case "moonshot-cn":
			return fmt.Sprintf("Moonshot 认证失败%s", statusMessage)
		case "opencode-go":
			return fmt.Sprintf("OpenCode Go 认证失败%s，请检查 Key 是否有效", statusMessage)
		default:
			return fmt.Sprintf("认证失败%s，请检查 Key 是否有效", statusMessage)
		}
	}
	if provider.IsRateLimited(err) {
		return "请求被上游限流，请稍后重试"
	}
	if provider.IsParseError(err) {
		switch p.ID() {
		case "deepseek":
			if keyType == provider.UsageTypeBalance {
				return "DeepSeek 上游响应异常，无法解析余额数据"
			}
			return "DeepSeek 上游响应异常，请稍后重试"
		case "minimax":
			return "MiniMax 上游响应异常，无法解析套餐数据"
		case "openai":
			return "OpenAI 上游响应异常"
		case "mimo":
			return "MiMo 上游响应异常，无法解析用量数据"
		case "moonshot-cn":
			return "Moonshot 上游响应异常"
		default:
			return "上游响应异常，请稍后重试"
		}
	}
	if errors.Is(err, provider.ErrRetriesExhausted) {
		return "上游暂不可用（已重试仍失败），请稍后重试"
	}
	if provider.IsNotSupported(err) {
		return "此提供商不支持该类型的用量查询"
	}
	if providerErrHasStatus(err) || errors.Is(err, provider.ErrUpstream) {
		if p.ID() == "opencode-go" && status == http.StatusForbidden {
			return "OpenCode Go 拒绝访问（HTTP 403），请检查套餐或权限条件"
		}
		if status != 0 {
			if name := upstreamDisplayName(p.ID()); name != "" {
				return fmt.Sprintf("%s 上游暂不可用（HTTP %d），请稍后重试", name, status)
			}
			return fmt.Sprintf("上游暂不可用（HTTP %d），请稍后重试", status)
		}
		return "上游暂不可用，请稍后重试"
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return "网络连接失败，请检查网络后重试"
	}
	return "上游暂不可用，请稍后重试"
}

func upstreamDisplayName(id string) string {
	switch id {
	case "opencode-go":
		return "OpenCode Go"
	case "deepseek":
		return "DeepSeek"
	case "minimax":
		return "MiniMax"
	case "openai":
		return "OpenAI"
	case "mimo":
		return "MiMo"
	case "moonshot-cn":
		return "Moonshot"
	case "kimi-code":
		return "Kimi Code"
	case "zai":
		return "智谱"
	default:
		return ""
	}
}

func providerErrHasStatus(err error) bool { return httpStatusFromError(err) != 0 }

func httpStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	message := err.Error()
	index := strings.Index(message, "HTTP ")
	if index < 0 || index+8 > len(message) {
		return 0
	}
	status, parseErr := strconv.Atoi(message[index+5 : index+8])
	if parseErr != nil {
		return 0
	}
	return status
}

// mergeUsage 合并 balance 与 plan 两个 Usage：balance 贡献 Balance/BalanceType，
// plan 贡献 Plan；两者 Quotas 都保留（append）。BalanceType 取有值的一侧（默认 balance）。
func mergeUsage(bal, plan *provider.Usage) *provider.Usage {
	merged := &provider.Usage{
		BalanceType: provider.UsageTypeBalance,
		UpdatedAt:   time.Now(),
	}
	if bal != nil {
		if bal.BalanceType != "" {
			merged.BalanceType = bal.BalanceType
		}
		merged.Balance = bal.Balance
		merged.Quotas = append(merged.Quotas, bal.Quotas...)
		merged.Raw = bal.Raw
	}
	if plan != nil {
		merged.Plan = plan.Plan
		merged.Quotas = append(merged.Quotas, plan.Quotas...)
		if merged.Raw == nil {
			merged.Raw = plan.Raw
		}
	}

	// 按时间窗口从小到大排序：5h → daily → weekly → monthly → 其他
	sortQuotas(merged)

	return merged
}

// joinError 拼接两条错误信息，任一为空时只保留非空侧。
func joinError(existing, added string) string {
	if existing == "" {
		return added
	}
	if added == "" {
		return existing
	}
	return existing + "；" + added
}

// periodRank 返回配额周期的排序权重（越小越靠前）。
func periodRank(period string) int {
	switch period {
	case "5h":
		return 0
	case "daily":
		return 1
	case "weekly":
		return 2
	case "monthly":
		return 3
	default:
		return 100
	}
}

// normalizeQuotaPercents 把配额剩余百分比收敛为最多两位小数，
// 消除 FormatFloat('f',-1) 可能产生的浮点长串（如 60.099999999999994）。
// 非数值/空串原样保留；幂等。
func normalizeQuotaPercents(u *provider.Usage) {
	if u == nil {
		return
	}
	for i := range u.Quotas {
		p := u.Quotas[i].Percent
		if p == "" {
			continue
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		u.Quotas[i].Percent = strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
	}
}

// sortQuotas 按时间窗口从小到大排序配额：5h → daily → weekly → monthly → 其他。
func sortQuotas(u *provider.Usage) {
	if u == nil || len(u.Quotas) <= 1 {
		return
	}
	sort.Slice(u.Quotas, func(i, j int) bool {
		return periodRank(u.Quotas[i].Period) < periodRank(u.Quotas[j].Period)
	})
}

// usageCacheKey 构造缓存 key。key id 全局唯一，provider 冗余便于调试。
func (s *Server) usageCacheKey(rec store.KeyRecord) string {
	return fmt.Sprintf("usage:%d:%s", rec.ID, rec.Provider)
}

// forceTTL 返回查询该 provider 应使用的缓存时长。
// modelscope 每次查询消耗 1 次请求额度，必须强制 1h 缓存（learnings T12）。
func (s *Server) forceTTL(p provider.Provider) time.Duration {
	if p.ID() == "modelscope" {
		return modelscopeTTL
	}
	return s.cacheTTL
}

// pathID 解析路径参数 {id} 为 int64。
func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// decodeJSON 解析请求体（上限 1MB，防超大 body 拖垮内存）。
func decodeJSON(r *http.Request, v interface{}) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 写统一错误响应 {"error":"..."}。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
