# 项目导航

AI 用量助手：用 Go 提供本地 HTTP API、网页和多家 AI 提供商用量适配。

## 技术栈

Go（版本见 `go.mod`）、标准库 `http.ServeMux`、纯 Go SQLite（无 CGO）、嵌入式单页原生 JS（无前端构建）。

## 目录导航

| 目录 | 入口 / 职责 |
|---|---|
| `cmd/gateway` | 程序入口与 `serve` 服务装配。 |
| `internal/server` | `api.go` 路由与 API；`snapshot.go` 页面初始数据注入；`middleware.go` 认证、Origin 校验与 recover。 |
| `internal/web` | `handler.go` 将单页 HTML 嵌入二进制并渲染。 |
| `internal/provider` | `spec.go` / `registry.go` provider 元数据与注册；各 provider 子目录实现查询；`types.go` 数据结构；`errors.go` 错误哨兵。 |
| `internal/store` | SQLite 本地存储。 |
| `internal/cache` | 用量 TTL 缓存与 singleflight。 |
| `internal/config` | 内置 flag 与环境变量运行配置。 |
| `internal/appconf` | 用户 `config.json` 两级加载（用户级 → 系统级，默认生成在用户级）；`ProjectName` 常量位于此处。 |

## 关键约定

- 启动服务使用 `gw serve` 子命令；直接运行不启动 HTTP 服务。
- 用量查询的局部失败惯例为 HTTP 200，错误放在 `Usage.Error` / `Usage.ErrorCode`；凭证错误码有 `credential_missing`、`credential_expired`。
- Key 在列表与网页脱敏；`GET /api/keys/{id}/key` 是用户主动读取完整 Key 的例外。
- 新增 provider：编写 `Spec` 和 `FetchUsage` 查询逻辑，并在注册处挂载；细节以 `internal/provider/spec.go` 为准。
- 内置 flag 配置（`internal/config`）与用户功能 `config.json`（`internal/appconf`）彼此独立，勿混淆。

## 常用命令

```bash
go build ./...
go test ./...
gofmt -l .
```

涉及项目名路径时以 `internal/appconf.ProjectName` 为准。行为变更时同步更新 `README.md`。
