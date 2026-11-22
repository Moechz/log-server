// Package auth 复用 TOS 平台的用户与会话（D-008）。
//
// 官方规范：TOS 7 Application Development Guide §8.9 / §8.10（均为 WebUI Internal Open 章节）。
// 平台会话 cookie 为 `TMSESSNAME`，另有 `X-Csrf-Token` 用于 CSRF 一致性校验。
//
// 本实现同时接受**两条通道**，以兼容两种打开方式：
//
//	A. 标准 Cookie 头（External Open 同源，浏览器自动携带）—— 我们的默认形态（D-018）
//	B. 自定义 `Cookie` 头 + `X-Csrf-Token` 头（§8.10 约定，Internal Open 用）
//
// 安全前提：管理端口只绑回环、仅平台 nginx 可达；因此**不能**用"来源是 127.0.0.1"来放行 ——
// nginx 反代同样来自回环。必须真的校验会话（或显式配置 auth=off 用于本地开发）。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mode 是鉴权模式。
type Mode string

const (
	// ModeTOS 要求 TOS 会话（生产默认）
	ModeTOS Mode = "tos"
	// ModeOff 关闭鉴权（**仅限本地开发**；启动时会打醒目告警）
	ModeOff Mode = "off"
)

// Role 是应用内角色（D-008 四角色）。
type Role string

const (
	RoleAdmin    Role = "admin"    // 全部权限
	RoleOperator Role = "operator" // 采集/转发/告警
	RoleViewer   Role = "viewer"   // 只读
	RoleAuditor  Role = "auditor"  // 仅审计数据 + 导出
)

// User 是已认证用户。
type User struct {
	Name      string   `json:"name"`
	Role      Role     `json:"role"`
	Source    string   `json:"source"` // tos | local | dev
	SessionID string   `json:"-"`
	IsAdmin   bool     `json:"is_admin"`
	Groups    []string `json:"groups,omitempty"`
}

// Can 判断角色是否允许某类操作。
func (u User) Can(op string) bool {
	switch u.Role {
	case RoleAdmin:
		return true
	case RoleOperator:
		return op != "storage.settings" && op != "users.manage" && op != "audit.lock"
	case RoleAuditor:
		return strings.HasPrefix(op, "audit.") || op == "read" || op == "export"
	case RoleViewer:
		return op == "read"
	}
	return false
}

// Config 鉴权配置。
type Config struct {
	Mode Mode
	// Admins 是映射为 admin 的 TOS 用户名（TOS 侧没有可用的角色接口前使用）。
	// 注意：TOS 用户列表接口尚未在开发文档中提供 → 见 TaskState 的跟进项。
	Admins []string
	// ValidateSession 可选：把会话交给平台校验（官方文档提到"Validate session via TOS
	// platform"但未给出端点）。为 nil 时只做**存在性 + CSRF 一致性**校验（与官方 Go 示例一致）。
	ValidateSession func(sessionID string) (User, error)
	// AdminToken 是本地管理员令牌（<DataDir>/config/admin.token，0600）。
	// 平台没有"查询当前用户/角色"接口 → 用显式令牌把"能改配置的人"与"普通 TOS 用户"分开。
	// 为空 = 管理员操作不可用（所有 TOS 会话按 operator），不会回退为"所有人管理员"。
	AdminToken string
	// ClaimPath 是"首个管理员已认领"标记（<DataDir>/config/admin.claimed）。
	// 首次安装后允许第一个通过 TOS 登录的用户一键认领管理员；之后只能读 admin.token 解锁。
	ClaimPath string
	// CookiePath 是认领 cookie 的作用域（默认 "/"；生产应设为 "/<appid>/"，
	// 避免同主机上的其他应用后端收到该 cookie）。
	CookiePath string
}

// ErrAlreadyClaimed / ErrAdminDisabled 供 HTTP 层映射为 409 / 503。
var (
	ErrAlreadyClaimed = fmt.Errorf("auth: 管理员已被认领")
	ErrAdminDisabled  = fmt.Errorf("auth: 管理员令牌未启用")
)

// Authenticator 负责从请求中解析并校验会话。
type Authenticator struct {
	cfg Config

	mu sync.RWMutex
	// 简单失败计数（便于排查"一直 401"是否被扫描）
	denied uint64
}

// New 创建鉴权器。
func New(cfg Config) *Authenticator {
	if cfg.Mode == "" {
		cfg.Mode = ModeTOS
	}
	return &Authenticator{cfg: cfg}
}

// Mode 返回当前模式。
func (a *Authenticator) Mode() Mode { return a.cfg.Mode }

// Denied 返回拒绝次数。
func (a *Authenticator) Denied() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.denied
}

// Authenticate 解析请求中的会话。
// 返回 (user, ok)。ok=false 时调用方应返回 401（前端据此跳回 TOS 登录页）。
func (a *Authenticator) Authenticate(r *http.Request) (User, bool) {
	if a.cfg.Mode == ModeOff {
		return User{Name: "dev", Role: RoleAdmin, Source: "dev", IsAdmin: true}, true
	}
	// 本地管理员令牌优先：它不依赖平台身份，用于解锁配置/存储/导出等管理操作。
	// 令牌只存在数据卷 0600 文件（或显式 env），不进入日志/诊断包。
	if a.tokenOK(r) {
		return User{Name: "local-admin", Role: RoleAdmin, Source: "local", IsAdmin: true}, true
	}

	session, csrfFromCookie, csrfHeader, err := extractSession(r)
	if err != nil {
		a.deny()
		return User{}, false
	}
	if session == "" {
		a.deny()
		return User{}, false
	}
	// CSRF 一致性：两者都存在时必须相等（§8.10）
	if csrfHeader != "" && csrfFromCookie != "" && csrfHeader != csrfFromCookie {
		a.deny()
		return User{}, false
	}

	if a.cfg.ValidateSession != nil {
		u, err := a.cfg.ValidateSession(session)
		if err != nil {
			a.deny()
			return User{}, false
		}
		u.Source = "tos"
		u.SessionID = session
		return u, true
	}

	// 未提供平台校验端点时：会话存在 + CSRF 一致即通过，按配置映射角色。
	// 诚实说明：这只能证明"请求携带了一个会话 id"，**不能证明会话有效**；
	// 有效性由平台网关保证（我们只监听回环，只有网关可达）。
	// TOS 侧暂无"查询当前用户/角色"的公开接口 → 跟进项见 docs/TASK_STATE.md。
	role := a.cfg.defaultRole()
	return User{
		Name: "tos-session", Role: role, Source: "tos",
		SessionID: session, IsAdmin: role == RoleAdmin,
	}, true
}

// defaultRole 决定没有本地管理员令牌时的 TOS 会话角色。
//
// 默认 operator（最小权限）：TOS 没有"查询当前用户/角色"的公开接口，无法把具体用户
// 映射为管理员；因此管理操作改由**显式管理员令牌**解锁（见 AdminToken）。
// 兼容旧配置：Admins 首个元素为 "*" 时仍表示"所有已登录用户按管理员"（不推荐）。
func (c Config) defaultRole() Role {
	if len(c.Admins) > 0 && c.Admins[0] == "*" {
		return RoleAdmin
	}
	return RoleOperator
}

// tokenOK 校验 X-Logserver-Admin-Token（常量时间比较）。
func (a *Authenticator) tokenOK(r *http.Request) bool {
	want := strings.TrimSpace(a.cfg.AdminToken)
	if want == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Logserver-Admin-Token"))
	if got == "" {
		// 首次认领后会下发 HttpOnly cookie；之后的请求无需 JS 再带头。
		if c, err := r.Cookie("LSADMIN"); err == nil {
			got = strings.TrimSpace(c.Value)
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Claimed 返回管理员是否已被首次认领（标记文件存在）。
func (a *Authenticator) Claimed() bool {
	if a.cfg.ClaimPath == "" {
		return true // 未配置标记路径时视为已认领：只能走令牌，不允许一键认领
	}
	_, err := os.Stat(a.cfg.ClaimPath)
	return err == nil
}

// Claim 执行首次管理员认领：写标记（O_EXCL 防并发）并下发 HttpOnly cookie。
// 只允许在"尚未认领"且令牌已配置时调用；成功后该浏览器获得管理员。
func (a *Authenticator) Claim(w http.ResponseWriter) error {
	if strings.TrimSpace(a.cfg.AdminToken) == "" {
		return ErrAdminDisabled
	}
	if a.cfg.ClaimPath == "" {
		return ErrAlreadyClaimed
	}
	if err := os.MkdirAll(filepath.Dir(a.cfg.ClaimPath), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(a.cfg.ClaimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return ErrAlreadyClaimed
		}
		return err
	}
	_, _ = fmt.Fprintf(f, "claimed_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	_ = f.Close()
	_ = os.Chmod(a.cfg.ClaimPath, 0o600)
	path := a.cfg.CookiePath
	if path == "" {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: "LSADMIN", Value: a.cfg.AdminToken,
		Path: path, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// AdminTokenPath 返回管理员令牌的约定路径（供 main 与文档共用）。
func AdminTokenPath(configDir string) string { return filepath.Join(configDir, "admin.token") }

// LoadOrCreateAdminToken 读取（必要时生成）管理员令牌。
//
//   - 环境变量 LOGSERVER_ADMIN_TOKEN 优先（自动化/测试；不要在正式环境写进 env 文件）；
//   - 文件不存在/内容过短 → 生成 32 字节随机 hex，原子写入 0600；
//   - 返回 created=true 表示本次新生成（调用方只记录**路径**，绝不记录令牌）。
func LoadOrCreateAdminToken(path string, getenv func(string) string) (token string, created bool, err error) {
	if getenv != nil {
		if v := strings.TrimSpace(getenv("LOGSERVER_ADMIN_TOKEN")); v != "" {
			if len(v) < 16 {
				return "", false, fmt.Errorf("LOGSERVER_ADMIN_TOKEN 过短（至少 16 字符）")
			}
			return v, false, nil
		}
	}
	if b, rerr := os.ReadFile(path); rerr == nil {
		if v := strings.TrimSpace(string(b)); len(v) >= 16 {
			_ = os.Chmod(path, 0o600) // 纠正历史权限（不阻断启动）
			return v, false, nil
		}
	} else if !os.IsNotExist(rerr) {
		return "", false, rerr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", false, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	tok := hex.EncodeToString(raw)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	return tok, true, nil
}

func (a *Authenticator) deny() {
	a.mu.Lock()
	a.denied++
	a.mu.Unlock()
}

// extractSession 从两条通道提取会话与 CSRF。
//   - 标准 Cookie（External Open 同源）
//   - 自定义 `Cookie` 头（§8.10，Internal Open）
func extractSession(r *http.Request) (session, csrfCookie, csrfHeader string, err error) {
	csrfHeader = strings.TrimSpace(r.Header.Get("X-Csrf-Token"))

	// 通道 A：标准 Cookie
	if c, e := r.Cookie("TMSESSNAME"); e == nil && c.Value != "" {
		session = c.Value
	}
	if c, e := r.Cookie("X-Csrf-Token"); e == nil {
		csrfCookie = c.Value
	}

	// 通道 B：自定义 Cookie 头（优先级：标准 Cookie 已有则仅补缺）
	if raw := r.Header.Get("Cookie"); raw != "" {
		parts := parseCookieString(raw)
		if session == "" {
			session = parts["TMSESSNAME"]
		}
		if csrfCookie == "" {
			csrfCookie = parts["X-Csrf-Token"]
		}
	}

	if r.Header.Get("Cookie") == "" && session == "" {
		// 两条通道都没有
		return "", "", csrfHeader, nil
	}
	return session, csrfCookie, csrfHeader, nil
}

// parseCookieString 解析 `k=v; k2=v2`（§8.10 的自定义 Cookie 头格式）。
func parseCookieString(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}

// ExemptPaths 是无需鉴权的路径（健康检查与指标；均为本地/网关可达）。
var ExemptPaths = map[string]bool{
	"/healthz":     true,
	"/metrics":     true,
	"/favicon.ico": true,
}

// ShouldExempt 判断路径是否豁免（静态资源与健康检查）。
func ShouldExempt(p string) bool {
	if ExemptPaths[p] {
		return true
	}
	// 静态资源（前端页面与 assets）由平台 nginx/网关提供访问控制，且不含数据
	if strings.HasPrefix(p, "/assets/") {
		return true
	}
	if p == "/" || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js") ||
		strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".svg") {
		return true
	}
	return false
}

// FromEnv 读取鉴权模式相关环境变量。
func FromEnv(getenv func(string) string) Config {
	cfg := Config{Mode: ModeTOS}
	switch strings.ToLower(strings.TrimSpace(getenv("LOGSERVER_AUTH"))) {
	case "off", "none", "disabled":
		cfg.Mode = ModeOff
	case "tos", "":
		cfg.Mode = ModeTOS
	}
	if admins := strings.TrimSpace(getenv("LOGSERVER_AUTH_ADMINS")); admins != "" {
		cfg.Admins = strings.Split(admins, ",")
		for i := range cfg.Admins {
			cfg.Admins[i] = strings.TrimSpace(cfg.Admins[i])
		}
	}
	return cfg
}

// StartupWarning 返回启动时的安全提示（ModeOff 时必须在日志里醒目提示）。
func (a *Authenticator) StartupWarning() string {
	if a.cfg.Mode == ModeOff {
		return "⚠️ 鉴权已关闭（LOGSERVER_AUTH=off）—— 仅限本地开发使用，请勿在正式环境保持该设置"
	}
	return ""
}

// Unauthorized 写出 401 响应（含前端跳转提示）。
// 契约（§8.10）：后端在会话失效时返回 401，前端检测到 401 后跳回 `/` 触发 TOS 重新登录。
func Unauthorized(w http.ResponseWriter, reasons ...string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Auth-Redirect", "/") // 前端可据此跳转；亦兼容直接读 status
	w.WriteHeader(http.StatusUnauthorized)
	msg := "会话无效或已过期，请重新登录 TOS"
	if len(reasons) > 0 && reasons[0] != "" {
		msg = reasons[0]
	}
	_, _ = fmt.Fprintf(w, `{"ok":false,"error":{"code":"unauthorized","message":"%s"}}`, msg)
}

// Describe 便于测试与本地脚本读取当前模式说明。
func (a *Authenticator) Describe() map[string]any {
	return map[string]any{
		"mode": a.Mode(), "admins": a.cfg.Admins,
		"platform_validation": a.cfg.ValidateSession != nil,
		"admin_token":         a.cfg.AdminToken != "", // 只报告"是否启用"，绝不回显令牌
		"admin_token_path":    "config/admin.token",
		"admin_claimable":     a.cfg.AdminToken != "" && !a.Claimed(),
		"denied":              a.Denied(),
	}
}

// ── 请求上下文传递 ──────────────────────────────────────────────────

type ctxKey struct{}

// WithUser 把已认证用户放入请求上下文。
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext 取出已认证用户。
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}
