# AI 用量助手

AI 用量助手：统一查询多家 AI 提供商 API Key 的余额与套餐用量，并提供本地网页和 REST API。

## 快速开始

需要 Go **1.26.5 或更高版本**。

```bash
./build.sh   # 编译并安装到 ~/.local/bin/aiusage（需在 PATH 中）
aiusage serve
```

启动服务必须使用 `serve` 子命令。默认监听 `127.0.0.1:8080`，浏览器访问 <http://127.0.0.1:8080>。更多启动参数见 [CLI](#cli)。

## 功能列表

- 支持 10 家提供商的用量查询或凭证管理，支持情况见[提供商支持矩阵](#提供商支持矩阵)。
- 管理 API Key：添加、编辑、删除、排序；常规列表和页面中均脱敏显示。
- REST API 与网页共用数据，可在本机浏览器或通过 API 查看。
- 网页提供账号级凭证更新弹窗和提供商控制台快捷链接。
- 普通提供商用量默认缓存 5 分钟；ModelScope 固定缓存 1 小时。
- 本地优先：单二进制运行，数据存储于本地 SQLite，无云端服务依赖。

### 提供商支持矩阵

| 提供商 | ID | 查询方式 | 认证 | 用量类型 | 用量页直达 | 状态 |
|---|---|---|---|---|---|---|
| DeepSeek | `deepseek` | 余额（官方 API） | Bearer `sk-` | 余额 | [platform.deepseek.com/usage](https://platform.deepseek.com/usage) | ✅ 稳定 |
| Moonshot（Kimi 开放平台） | `moonshot-cn` | 余额（官方 API） | Bearer `sk-` | 余额 | [platform.kimi.com/console/account](https://platform.kimi.com/console/account) | ✅ 稳定 |
| Kimi Code | `kimi-code` | 套餐配额（社区逆向接口，未官方文档化） | Bearer `sk-kimi-` | 套餐：周 + 5h 窗口 | [www.kimi.com/code/console](https://www.kimi.com/code/console)（控制台内查看） | ⚠️ 逆向接口 |
| MiniMax | `minimax` | 套餐配额（Coding Plan，官方 FAQ） | Bearer `sk-` | 套餐：5h + 周百分比 | [platform.minimaxi.com/console/usage](https://platform.minimaxi.com/console/usage) | ⚠️ 字段随版本变更 |
| 智谱 ZAI（GLM） | `zai` | 余额（官方 API）+ 套餐配额（Coding Plan 逆向接口） | Bearer 或裸 key `32hex.16alnum` | 余额 + 总消费；套餐：5h + 周窗口 | 套餐 → [bigmodel.cn 套餐用量](https://www.bigmodel.cn/coding-plan/personal/overview)；余额 → [bigmodel.cn 财务总览](https://www.bigmodel.cn/finance-center/finance/overview) | ✅ 余额稳定 / ⚠️ 套餐逆向 |
| OpenCode Go | `opencode-go` | 套餐窗口（官方端点，验证中） | Bearer `sk-` | 套餐：5h + 周 + 月 | [opencode.ai workspace](https://opencode.ai/workspace)（控制台内查看） | ⚠️ 端点验证中 |
| OpenAI | `openai` | ChatGPT/Codex 套餐配额（WHAM 非公开端点）；API key 的 credit grants 仅兼容兜底 | Bearer OAuth access token（`eyJ...`）；`sk-` API key 仅兜底 | 套餐：5h + 周窗口；余额：credit grants（USD） | 余额 → [platform.openai.com/usage](https://platform.openai.com/usage)；套餐 → [chatgpt.com/codex/cloud/settings/analytics](https://chatgpt.com/codex/cloud/settings/analytics) | ⚠️ 非公开端点 |
| ModelScope 魔搭 | `modelscope` | 每日限流次数（发送最小推理请求，读取响应头） | Bearer `ms-` | 每日请求次数 | [modelscope.cn 个人中心](https://modelscope.cn/my)（免费额度） | ⚠️ 每次查询消耗 1 次额度 |
| 阿里云百炼 Token Plan | `bailian` | 不可用（官方未开放用量查询 API，ModelStudio/BSS OpenAPI 均需阿里云 AK/SK 签名） | `sk-sp-` 点分套餐 key（正则校验，与按量 `sk-` 隔离） | 仅存储 | 套餐 → [百炼控制台 我的订阅](https://bailian.console.aliyun.com/cn-beijing?tab=plan&commonbuy=1&orderType=buy#/efm/subscription/token-plan/personal) | ❌ 官方未开放 |
| Xiaomi MiMo | `mimo` | 不可用（官方无用量 API） | `sk-` / `tp-`（Token Plan 套餐 key） | 仅存储 | 余额 → [platform.xiaomimimo.com/console/balance](https://platform.xiaomimimo.com/console/balance)；套餐 → [platform.xiaomimimo.com/console/plan-manage](https://platform.xiaomimimo.com/console/plan-manage) | ❌ 官方未开放 |

套餐价格信息以官方订阅页为准。标为逆向或验证中的接口可能随提供商变化而失效。

## 网页

打开 `http://127.0.0.1:8080` 后可使用：

- **用量仪表盘**：按提供商展示余额、配额和套餐信息，可刷新全部或单张卡片，并聚合展示用量。
- **Key 管理**：添加、删除、编辑账户/备注、修改类型和拖拽排序。禁用的 Key 不参与用量查询和仪表盘显示。
- **凭证更新**：凭证缺失或失效时，可从用量卡片打开更新弹窗；弹窗提供凭证说明与快捷链接。凭证按提供商和账户保存。
- **控制台链接**：从卡片前往提供商用量页；没有直达用量页时显示用量说明。

## CLI

```text
aiusage serve [flags]
aiusage version
```

| `serve` 参数 | 默认值 | 对应环境变量 | 说明 |
|---|---|---|---|
| `--port` | `8080` | `AI_USAGE_PORT` | HTTP 监听端口。 |
| `--listen` | `127.0.0.1` | `AI_USAGE_LISTEN` | 监听地址；可指定 `0.0.0.0` 或局域网 IP 对外访问。 |
| `--passwd` | 空（不启用认证） | `AI_USAGE_PASSWD` | 启用 Basic Auth，用户名固定为 `admin`。 |
| `--data-dir` | `~/.local/share/ai-usage` | `AI_USAGE_DATA_DIR` | SQLite 数据目录。 |
| `--cache-ttl` | `5m` | `AI_USAGE_CACHE_TTL` | 普通提供商用量缓存时长，例如 `5m`、`30s`。 |

显式 flag 优先于环境变量；未设置时使用默认值。`version` 打印版本号。

## API

提供同源 REST API，路径以 `/api/` 开头，使用 JSON 格式，可通过 curl 访问。设置 `--passwd` 后需使用 Basic Auth（用户名 `admin`，密码为配置值）。例如，查询全部用量：`curl -u admin:密码 http://127.0.0.1:8080/api/usage`。

## 配置

`config.json` 是控制网页功能和仪表盘卡片的用户级配置；它与 `serve` 的 `--port`、`--passwd`、`--data-dir` 等运行参数是两套独立配置。

程序按以下顺序查找配置，使用**第一个存在的文件**：

1. 可执行文件所在目录的 `config.json`
2. `~/.config/ai-usage/config.json`
3. `/etc/ai-usage/config.json`

三处都不存在时，程序尝试在可执行文件所在目录创建默认 `config.json`；无法创建时仍使用内置默认值。

完整示例（默认值全部开启）：

```json
{
  "dashboard": {
    "enabled": true,
    "cardFilter": {
      "mode": "blacklist",
      "cards": []
    }
  },
  "keys": {
    "enabled": true
  }
}
```

| JSON 字段 | 类型 / 可选值 | 说明 |
|---|---|---|
| `dashboard.enabled` | boolean，默认 `true` | 是否启用用量仪表盘。关闭后网页隐藏仪表盘，相关用量 API 返回 HTTP 403。 |
| `dashboard.cardFilter.mode` | `blacklist`（默认）/ `whitelist` | 卡片过滤模式。 |
| `dashboard.cardFilter.cards` | 字符串数组，默认 `[]` | 匹配 `provider` 或 `provider/account`。空数组表示不过滤。 |
| `keys.enabled` | boolean，默认 `true` | 是否启用 Key 管理。关闭后网页隐藏 Key 管理，写 API 返回 HTTP 403；只读接口仍可用。 |

卡片匹配区分大小写并按完整字符串精确匹配。`blacklist` 隐藏命中项，`whitelist` 只显示命中项；空列表在两种模式下都不过滤。两项功能都关闭时，网页显示功能均已关闭的提示。修改配置后需重启生效。

## 安全与 FAQ

### 数据与安全

- SQLite 数据库默认位于 `~/.local/share/ai-usage/gateway.db`，可通过 `--data-dir` 改变数据目录。Key 和账号级凭证保存在本地；Key 列表、网页及凭证查询接口会脱敏显示，但 `GET /api/keys/{id}/key` 会按需返回完整 Key。数据库中的 Key 与凭证是明文存储，请保护数据目录及其备份。
- 默认仅监听 `127.0.0.1`。写请求（POST、PATCH、DELETE）会做 Origin 同源校验；没有 Origin 的命令行请求放行。设置 `--passwd` 后，所有请求均要求 Basic Auth（`admin` + 所设密码）；密码用于访问控制，不加密数据库。
- 如需对外监听，请设置 `--listen` 并同时启用 `--passwd`。

### 常见问题

**凭证失效后如何更新？** 在仪表盘对应卡片点击“更新凭证”，按弹窗说明填写新凭证并保存。更新后相关账号用量缓存会失效并重新查询。

**用量缓存多久更新？** 普通提供商默认 5 分钟，可通过 `--cache-ttl` 或 `AI_USAGE_CACHE_TTL` 调整。ModelScope 每次查询消耗 1 次请求额度，因此始终强制缓存 1 小时；频繁强制刷新会增加额度消耗。

**为什么 ModelScope 不能跟随普通缓存设置？** 其查询通过最小推理请求读取限流响应头，每次调用都会消耗一次请求额度，故服务端固定使用 1 小时缓存。
