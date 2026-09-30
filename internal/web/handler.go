// Package web 提供网页界面渲染逻辑（html/template）。
//
// 模板用 //go:embed 嵌入二进制，部署零外部文件。Handler 持有
// *server.Server，渲染时调用 Snapshot 拉取初始数据注入模板。
// web 路由与 API 路由必须挂在同一 mux（同源），否则浏览器写请求
// 会被同源中间件（T15）拒绝。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"ai-usage/internal/server"
)

//go:embed templates/*.html
var templatesFS embed.FS

// indexTmpl 使用 template.JS 注入 JSON，避免 html/template 默认转义破坏 JSON。
// 动态数据由前端 JS 用 escapeText 转义后展示，模板本身只含静态结构。
var indexTmpl = template.Must(template.New("index.html").ParseFS(templatesFS, "templates/index.html"))

// Handler 是网页路由的 http.Handler。
type Handler struct {
	server *server.Server
}

// NewHandler 构造 web handler。返回的 handler 只响应 GET /，其他路径 404。
func NewHandler(s *server.Server) *Handler {
	return &Handler{server: s}
}

// ServeHTTP 实现 http.Handler。GET / 渲染 index.html，其他路径 404。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	snapshot, err := h.server.Snapshot(ctx)
	if err != nil {
		http.Error(w, "加载页面失败", http.StatusInternalServerError)
		return
	}

	dataJSON, err := json.Marshal(snapshot)
	if err != nil {
		http.Error(w, "序列化失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = indexTmpl.Execute(w, struct {
		InitialDataJSON template.JS
	}{InitialDataJSON: template.JS(dataJSON)})
}
