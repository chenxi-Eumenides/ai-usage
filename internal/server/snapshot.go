package server

import (
	"context"
	"time"

	"ai-usage/internal/appconf"
	"ai-usage/internal/provider"
)

// KeyView 是 web 页面展示用的 key 视图（keyResponse 的导出镜像）。
type KeyView struct {
	ID                  int64        `json:"id"`
	Provider            string       `json:"provider"`
	ProviderDisplayName string       `json:"provider_display_name"`
	KeyMasked           string       `json:"key_masked"`
	KeyType             string       `json:"key_type"`
	Account             string       `json:"account,omitempty"`
	Note                string       `json:"note,omitempty"`
	CreatedAt           string       `json:"created_at"`
	ConsoleURL          string       `json:"console_url"`
	PlanMeta            PlanMetaView `json:"plan_meta"`
}

// PlanMetaView 是 provider.PlanMeta 的导出 JSON 镜像（源类型无 json tag）。
type PlanMetaView struct {
	PlanName  string `json:"plan_name"`
	PlanPrice string `json:"plan_price"`
	ApiPrice  string `json:"api_price"`
}

// UsageView 是 web 页面展示用的单 key 用量视图（usageResult 的导出镜像）。
type UsageView struct {
	KeyID               int64           `json:"key_id"`
	Provider            string          `json:"provider"`
	ProviderDisplayName string          `json:"provider_display_name"`
	Account             string          `json:"account,omitempty"`
	UsageType           string          `json:"usage_type"`
	Usage               *provider.Usage `json:"usage,omitempty"`
	Error               string          `json:"error,omitempty"`
	ConsoleURL          string          `json:"console_url"`
	ConsoleUsageURL     string          `json:"console_usage_url,omitempty"`
	Skip                bool            `json:"skip,omitempty"`
}

// ProviderView 是「添加 key」表单下拉框需要的 provider 条目。
type ProviderView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// PageSnapshot 是 web 页面初始渲染所需的全部数据。
type PageSnapshot struct {
	Keys      []KeyView        `json:"keys"`
	Usage     []UsageView      `json:"usage"`
	UpdatedAt time.Time        `json:"updated_at"`
	Providers []ProviderView   `json:"providers"`
	Features  appconf.Features `json:"features"`
}

// Snapshot 聚合 web 页面初始数据。
//
// Usage 恒为空数组：页面首屏只渲染 key 列表，各卡片用量由前端并行逐卡片
// 拉取（GET /api/keys/{id}/usage）。若在此同步 fetchAllUsage，任一 provider
// 卡住会拖挂整个页面渲染（handler 有 5s 超时，慢 provider 直接导致页面 500）。
func (s *Server) Snapshot(ctx context.Context) (PageSnapshot, error) {
	records, err := s.store.ListKeys()
	if err != nil {
		return PageSnapshot{}, err
	}

	keys := make([]KeyView, 0, len(records))
	for _, rec := range records {
		k := s.toKeyResponse(rec)
		keys = append(keys, KeyView{
			ID:                  k.ID,
			Provider:            k.Provider,
			ProviderDisplayName: k.ProviderDisplayName,
			KeyMasked:           k.KeyMasked,
			KeyType:             k.KeyType,
			Account:             k.Account,
			Note:                k.Note,
			CreatedAt:           k.CreatedAt,
			ConsoleURL:          k.ConsoleURL,
			PlanMeta: PlanMetaView{
				PlanName:  k.PlanMeta.PlanName,
				PlanPrice: k.PlanMeta.PlanPrice,
				ApiPrice:  k.PlanMeta.ApiPrice,
			},
		})
	}

	allProviders := provider.All()
	providers := make([]ProviderView, 0, len(allProviders))
	for _, p := range allProviders {
		providers = append(providers, ProviderView{
			ID:          p.ID(),
			DisplayName: p.DisplayName(),
		})
	}

	return PageSnapshot{
		Keys:      keys,
		Usage:     []UsageView{},
		UpdatedAt: time.Now(),
		Providers: providers,
		Features:  s.appConf.PageFeatures(),
	}, nil
}
