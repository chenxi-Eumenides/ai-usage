package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"time"
)

// Wrap 按「外层 → 内层」顺序包裹：Recover → Logging → Auth（passwd 非空时）→ OriginCheck → h。
// 即请求先经 recover 捕获 panic，再经 logging 记访问日志，
// 认证启用时经 auth 校验 Basic Auth 凭证，最后经 originCheck 做同源校验后到达 h。
// 返回的 handler 保证：写请求经过同源校验、任何 panic 转为 500、
// 每条请求打一条不包含敏感信息的访问日志。
func Wrap(h http.Handler, passwd string) http.Handler {
	inner := originCheckMiddleware(h)
	if passwd != "" {
		inner = authMiddleware(inner, passwd)
	}
	return recoverMiddleware(loggingMiddleware(inner))
}

// ---------------------------------------------------------------------------
// Auth：可选 Basic Auth（用户名固定 admin，密码常量时间比较）
// ---------------------------------------------------------------------------

// authMiddleware 启用 Basic Auth 访问控制：用户名必须为 admin，
// 密码与 passwd 通过 SHA-256 抹平长度差异后常量时间比较（防时序攻击）。
// 校验失败返回 401 并携带 WWW-Authenticate 挑战头，浏览器会弹出登录框。
// 该中间件对所有方法生效（包括 GET），与 OriginCheck 只拦写方法不同。
func authMiddleware(next http.Handler, passwd string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || user != "admin" || !secureEqual(pw, passwd) {
			w.Header().Set("WWW-Authenticate", `Basic realm="ai-usage"`)
			writeError(w, http.StatusUnauthorized, "未授权")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureEqual 常量时间比较两个字符串。先对两者做 SHA-256，
// 使不同长度的输入产生相同长度的摘要参与比较，避免长度差异泄露时序信息。
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---------------------------------------------------------------------------
// OriginCheck：写操作同源校验（CSRF 防护）
// ---------------------------------------------------------------------------

// originCheckMiddleware 校验写请求（POST/PATCH/DELETE）的 Origin 头。
//
// 校验规则：
//   - 读方法（GET）→ 直接放行
//   - 无 Origin 头（curl/CLI 等非浏览器客户端）→ 放行
//   - Origin 存在且其 host[:port] 与请求 Host 一致 → 同源放行
//   - Origin 存在但 host 不匹配 → 403
//
// 注意：host 比较忽略 scheme，按 host[:port] 字符串精确比较
// （localhost 与 127.0.0.1 视为不同）。反向代理部署时需确保
// proxy_set_header Host $host 保持原始 Host，否则 Origin 校验会失败。
//
// 不设置 Access-Control-Allow-*：本服务不做跨域授权（无 CORS）。
func originCheckMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodDelete:
			// 继续下方校验
		default:
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			// 无 Origin 头：curl/CLI/服务端调用，非浏览器环境，放行。
			next.ServeHTTP(w, r)
			return
		}

		if !originHostMatches(origin, r.Host) {
			log.Printf("origin check failed: Origin=%q Host=%q", origin, r.Host)
			writeError(w, http.StatusForbidden, "跨站请求被拒绝")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originHostMatches 解析 Origin 的 host[:port] 并与请求 Host 比较。
// 比较 host 部分，端口仅在非默认端口时参与比较
// （http:80、https:443 视为无端口）。
func originHostMatches(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}

	originHost := u.Hostname()
	originPort := u.Port()

	reqHost, reqPort := splitHostPort(host)

	if originHost != reqHost {
		return false
	}

	// 端口为空或为 scheme 默认端口时视为等价
	originPort = normalizePort(originPort, u.Scheme)
	reqPort = normalizePort(reqPort, u.Scheme)
	return originPort == reqPort
}

func splitHostPort(host string) (h, p string) {
	// net.SplitHostPort 会把无端口的 host 当作 error，
	// 此时整个字符串就是 host。
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return host, ""
	}
	return h, p
}

func normalizePort(port, scheme string) string {
	if port == "" {
		return ""
	}
	if scheme == "https" && port == "443" {
		return ""
	}
	if scheme == "http" && port == "80" {
		return ""
	}
	return port
}

// ---------------------------------------------------------------------------
// Recover：panic → 500，进程不崩
// ---------------------------------------------------------------------------

// recoverMiddleware 捕获下游 panic，记录堆栈（不含 key 等请求数据），
// 返回 500 中文错误信息。进程不退出。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered: %v\n%s", rec, debug.Stack())
				writeError(w, http.StatusInternalServerError, "服务内部发生错误，请稍后重试；若持续出现请查看服务日志")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Logging：脱敏访问日志
// ---------------------------------------------------------------------------

// statusRecorder 包装 ResponseWriter，捕获写入的状态码供日志使用。
// 默认 200：handler 不显式 WriteHeader 时按 200 记。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware 每条请求打一条日志：method path status duration。
// 绝不记录 Authorization 头、请求体、查询参数、key 等敏感信息。
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}
