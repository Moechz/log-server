package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func reqWith(cookieHeader, csrfHeader string, cookies map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/api/v1/overview", nil)
	if cookieHeader != "" {
		r.Header.Set("Cookie", cookieHeader)
	}
	if csrfHeader != "" {
		r.Header.Set("X-Csrf-Token", csrfHeader)
	}
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return r
}

func TestAuthenticate_ModeOff(t *testing.T) {
	a := New(Config{Mode: ModeOff})
	u, ok := a.Authenticate(reqWith("", "", nil))
	if !ok || u.Role != RoleAdmin || u.Source != "dev" {
		t.Fatalf("off 模式应放行并给 admin/dev，得到 %+v ok=%v", u, ok)
	}
	if a.StartupWarning() == "" {
		t.Error("off 模式必须返回启动告警（不能静默）")
	}
}

func TestAuthenticate_RequiresSession(t *testing.T) {
	a := New(Config{Mode: ModeTOS})
	if _, ok := a.Authenticate(reqWith("", "", nil)); ok {
		t.Fatal("无会话必须拒绝")
	}
	if a.Denied() == 0 {
		t.Error("拒绝次数应被统计")
	}
}

func TestAuthenticate_StandardCookieChannel(t *testing.T) {
	a := New(Config{Mode: ModeTOS, Admins: []string{"*"}})
	u, ok := a.Authenticate(reqWith("", "", map[string]string{"TMSESSNAME": "sess-123"}))
	if !ok {
		t.Fatal("标准 cookie 通道应通过（External Open 同源）")
	}
	if u.SessionID != "sess-123" {
		t.Errorf("SessionID = %q", u.SessionID)
	}
	if !u.IsAdmin {
		t.Error("Admins=* 时应映射为管理员")
	}
}

func TestAuthenticate_CustomCookieHeaderChannel(t *testing.T) {
	// 默认最小权限：没有管理员令牌时，TOS 会话 = operator（只读/运维，不能改配置）。
	a := New(Config{Mode: ModeTOS})
	r := reqWith("TMSESSNAME=abc; X-Csrf-Token=tk1;", "tk1", nil)
	u, ok := a.Authenticate(r)
	if !ok {
		t.Fatal("§8.10 自定义 Cookie 头通道应通过")
	}
	if u.SessionID != "abc" {
		t.Errorf("会话解析错误: %q", u.SessionID)
	}
	if u.Role != RoleOperator || u.IsAdmin {
		t.Errorf("默认角色应为 operator（最小权限），得到 %q isAdmin=%v", u.Role, u.IsAdmin)
	}
}

func TestAuthenticate_DefaultIsOperator(t *testing.T) {
	a := New(Config{Mode: ModeTOS})
	u, ok := a.Authenticate(reqWith("TMSESSNAME=s1;", "", nil))
	if !ok {
		t.Fatal("有效会话应通过")
	}
	if u.Role != RoleOperator || u.IsAdmin {
		t.Fatalf("默认必须是 operator，得到 %+v", u)
	}
}

func TestAuthenticate_AdminToken(t *testing.T) {
	a := New(Config{Mode: ModeTOS, AdminToken: "0123456789abcdef0123456789abcdef"})
	r := reqWith("", "", nil)
	r.Header.Set("X-Logserver-Admin-Token", "0123456789abcdef0123456789abcdef")
	u, ok := a.Authenticate(r)
	if !ok || !u.IsAdmin || u.Role != RoleAdmin || u.Source != "local" {
		t.Fatalf("正确令牌应给 local admin，得到 %+v ok=%v", u, ok)
	}

	// 错误令牌且无 TOS 会话 → 拒绝（不能因为带了个头就放行）。
	bad := reqWith("", "", nil)
	bad.Header.Set("X-Logserver-Admin-Token", "wrong-token")
	if _, ok := a.Authenticate(bad); ok {
		t.Fatal("错误令牌 + 无会话必须拒绝")
	}

	// 错误令牌 + 有效 TOS 会话 → 回落为 operator（不是 admin）。
	fallback := reqWith("TMSESSNAME=s1;", "", nil)
	fallback.Header.Set("X-Logserver-Admin-Token", "wrong-token")
	u2, ok := a.Authenticate(fallback)
	if !ok || u2.IsAdmin || u2.Role != RoleOperator {
		t.Fatalf("错误令牌应回落 operator，得到 %+v ok=%v", u2, ok)
	}
}

func TestLoadOrCreateAdminToken(t *testing.T) {
	dir := t.TempDir()
	path := AdminTokenPath(dir)

	tok1, created, err := LoadOrCreateAdminToken(path, func(string) string { return "" })
	if err != nil || !created || len(tok1) < 32 {
		t.Fatalf("首次应生成令牌: tok=%q created=%v err=%v", tok1, created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("令牌文件权限应为 0600，得到 %v", fi.Mode().Perm())
	}

	tok2, created2, err := LoadOrCreateAdminToken(path, func(string) string { return "" })
	if err != nil || created2 || tok2 != tok1 {
		t.Fatalf("第二次应复用同一令牌: %q vs %q created=%v err=%v", tok2, tok1, created2, err)
	}

	// env 覆盖（自动化/测试用），且不要求文件存在。
	envTok := "env-token-0123456789abcdef"
	tok3, created3, err := LoadOrCreateAdminToken(filepath.Join(dir, "nope.token"), func(k string) string {
		if k == "LOGSERVER_ADMIN_TOKEN" {
			return envTok
		}
		return ""
	})
	if err != nil || created3 || tok3 != envTok {
		t.Fatalf("env 令牌应优先: %q created=%v err=%v", tok3, created3, err)
	}
}

func TestAdminTokenTooShortFromEnv(t *testing.T) {
	if _, _, err := LoadOrCreateAdminToken(t.TempDir()+"/t", func(string) string { return "short" }); err == nil {
		t.Fatal("过短的 env 令牌应报错")
	}
}

func TestAuthenticate_OperatorMappingWhenNotStar(t *testing.T) {
	// 显式配置非 "*" 的 admins → 落到 operator（为将来多用户角色做准备）
	a := New(Config{Mode: ModeTOS, Admins: []string{"someone-else"}})
	u, ok := a.Authenticate(reqWith("TMSESSNAME=x;", "", nil))
	if !ok {
		t.Fatal("应通过")
	}
	if u.Role != RoleOperator || u.IsAdmin {
		t.Errorf("应映射为 operator，得到 %+v", u)
	}
}

func TestAuthenticate_CsrfMismatchRejected(t *testing.T) {
	a := New(Config{Mode: ModeTOS})
	// 头里的 csrf 与 cookie 里的不一致 → 必须拒绝（防 CSRF）
	if _, ok := a.Authenticate(reqWith("TMSESSNAME=abc; X-Csrf-Token=tk1;", "tk2", nil)); ok {
		t.Fatal("CSRF 不一致必须拒绝")
	}
}

func TestAuthenticate_ValidateSessionHook(t *testing.T) {
	called := ""
	a := New(Config{
		Mode: ModeTOS,
		ValidateSession: func(s string) (User, error) {
			called = s
			return User{Name: "alice", Role: RoleAuditor}, nil
		},
	})
	u, ok := a.Authenticate(reqWith("TMSESSNAME=s9;", "", nil))
	if !ok || called != "s9" {
		t.Fatalf("平台校验钩子未被调用: called=%q ok=%v", called, ok)
	}
	if u.Name != "alice" || u.Role != RoleAuditor {
		t.Errorf("应使用钩子返回的用户: %+v", u)
	}
}

func TestAuthenticate_ValidateSessionFailureRejects(t *testing.T) {
	a := New(Config{Mode: ModeTOS, ValidateSession: func(string) (User, error) {
		return User{}, context.Canceled
	}})
	if _, ok := a.Authenticate(reqWith("TMSESSNAME=x;", "", nil)); ok {
		t.Fatal("平台校验失败必须拒绝")
	}
}

func TestRole_Can(t *testing.T) {
	cases := []struct {
		role, op string
		want     bool
	}{
		{"admin", "storage.settings", true},
		{"admin", "audit.lock", true},
		{"operator", "read", true},
		{"operator", "storage.settings", false},
		{"auditor", "audit.read", true},
		{"auditor", "read", true},
		{"auditor", "settings", false},
		{"viewer", "read", true},
		{"viewer", "export", false},
	}
	for _, c := range cases {
		if got := (User{Role: Role(c.role)}).Can(c.op); got != c.want {
			t.Errorf("%s 能否 %s = %v，期望 %v", c.role, c.op, got, c.want)
		}
	}
}

func TestShouldExempt(t *testing.T) {
	for _, p := range []string{"/healthz", "/metrics", "/assets/app.js", "/", "/index.html"} {
		if !ShouldExempt(p) {
			t.Errorf("%s 应豁免", p)
		}
	}
	for _, p := range []string{"/api/v1/overview", "/api/v1/logs", "/api/v1/capabilities"} {
		if ShouldExempt(p) {
			t.Errorf("%s 不应豁免（数据接口必须鉴权）", p)
		}
	}
}

func TestUnauthorized_ResponseContract(t *testing.T) {
	rec := httptest.NewRecorder()
	Unauthorized(rec)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码应为 401，得到 %d", rec.Code)
	}
	if rec.Header().Get("X-Auth-Redirect") != "/" {
		t.Error("应给出跳转提示头 X-Auth-Redirect: /")
	}
	if body := rec.Body.String(); body == "" || body[0] != '{' {
		t.Errorf("响应应为 JSON 信封: %s", body)
	}
}

func TestWithUser_RoundTrip(t *testing.T) {
	ctx := WithUser(context.Background(), User{Name: "bob", Role: RoleViewer})
	u, ok := FromContext(ctx)
	if !ok || u.Name != "bob" {
		t.Fatalf("上下文用户往返失败: %+v ok=%v", u, ok)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"LOGSERVER_AUTH": "off", "LOGSERVER_AUTH_ADMINS": "alice, bob"}
	cfg := FromEnv(func(k string) string { return env[k] })
	if cfg.Mode != ModeOff {
		t.Errorf("Mode = %q", cfg.Mode)
	}
	if len(cfg.Admins) != 2 || cfg.Admins[1] != "bob" {
		t.Errorf("Admins 解析错误: %v", cfg.Admins)
	}
	if d := FromEnv(func(string) string { return "" }); d.Mode != ModeTOS {
		t.Errorf("默认应为 tos，得到 %q", d.Mode)
	}
}

func TestClaim_OnceAndCookie(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{
		Mode: ModeTOS, AdminToken: "0123456789abcdef0123456789abcdef",
		ClaimPath: filepath.Join(dir, "admin.claimed"),
	})
	if a.Claimed() {
		t.Fatal("初始应为未认领")
	}
	rec := httptest.NewRecorder()
	if err := a.Claim(rec); err != nil {
		t.Fatalf("首次认领失败: %v", err)
	}
	if !a.Claimed() {
		t.Fatal("认领后标记应存在")
	}
	if fi, err := os.Stat(filepath.Join(dir, "admin.claimed")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("认领标记应为 0600: %v", err)
	}
	if err := a.Claim(httptest.NewRecorder()); err != ErrAlreadyClaimed {
		t.Fatalf("第二次认领应返回 ErrAlreadyClaimed，得到 %v", err)
	}

	// 认领时下发的 HttpOnly cookie 应能直接认证为 admin。
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != "LSADMIN" || !cookies[0].HttpOnly {
		t.Fatalf("应下发 HttpOnly LSADMIN cookie，得到 %+v", cookies)
	}
	req := reqWith("", "", nil)
	req.AddCookie(cookies[0])
	u, ok := a.Authenticate(req)
	if !ok || !u.IsAdmin || u.Source != "local" {
		t.Fatalf("认领 cookie 应认证为 local admin，得到 %+v ok=%v", u, ok)
	}

	// Describe 不再显示可认领。
	if d := a.Describe(); d["admin_claimable"] != false {
		t.Fatalf("认领后 admin_claimable 应为 false: %v", d)
	}
}

func TestClaim_DisabledWithoutToken(t *testing.T) {
	a := New(Config{Mode: ModeTOS, ClaimPath: filepath.Join(t.TempDir(), "admin.claimed")})
	if err := a.Claim(httptest.NewRecorder()); err != ErrAdminDisabled {
		t.Fatalf("无令牌时应拒绝认领，得到 %v", err)
	}
}
