package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Moechz/log-server/backend/internal/forward"
)

// FR-1003 / TC-13：诊断包必须内容完整且凭据/口令已脱敏。
func TestDiagnostics_ContentAndMasking(t *testing.T) {
	s := newTestServer(t)
	// 注入一个"带凭据"的转发目标与一条含 token 的自身日志
	s.ForwardStats = func() []forward.Status {
		return []forward.Status{{
			ID: "t1", Name: "remote", Protocol: forward.ProtoHTTP,
			Addr: "https://svcuser:s3cr3t@logs.example.com:9200", Enabled: true,
		}}
	}
	s.Self.Add("warn", "forward", "转发目标不可达", "url", "https://u:p@h/", "token", "ghp_abcdefghijklmnopqrst")

	code, body := do(t, s, "GET", "/api/v1/diagnostics", "")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("诊断包接口失败: %d %v", code, body)
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatal("缺少 data")
	}
	// 内容完整性：版本 / 运行时 / 健康 / 能力 / 配置 / 自身日志
	for _, k := range []string{"generated_at", "version", "runtime", "health",
		"capabilities", "config", "selflog", "forward", "auth", "masked_fields"} {
		if _, ok := data[k]; !ok {
			t.Errorf("诊断包缺少 %q 段", k)
		}
	}
	// 脱敏：转发地址里的 user:pass 与 token 都不得出现
	raw := mustJSON(t, data)
	for _, leak := range []string{"s3cr3t", "svcuser", "ghp_abcdefghijklmnopqrst", "u:p@"} {
		if strings.Contains(raw, leak) {
			t.Errorf("诊断包泄漏了凭据: %q\n%s", leak, raw)
		}
	}
	if !strings.Contains(raw, Masked) {
		t.Error("诊断包应包含脱敏占位符")
	}
	// 凭证文件路径也不能出现（只报"是否已配置"）
	cfg, _ := data["config"].(map[string]any)
	if _, ok := cfg["tls_cert_file"]; ok {
		t.Error("配置摘要不应包含证书路径字段")
	}
	if _, ok := cfg["tls_cert_configured"]; !ok {
		t.Error("配置摘要应报告证书是否已配置（布尔）")
	}
}

func TestMaskString_URLAndTokenPatterns(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@example.com:9200/logs": "https://***@example.com:9200/logs",
		"Bearer abcdefghijklmnopqrstuvwxyz123456": Masked,
		"ghp_ABCDEFGHIJKLMNOPQRST":                Masked,
		"plain host:514 (no credential)":          "plain host:514 (no credential)",
	}
	for in, want := range cases {
		if got := maskString(in); got != want {
			t.Errorf("maskString(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestMaskAny_KeyAndValueMasking(t *testing.T) {
	in := map[string]any{
		"password":  "hunter2",
		"api_key":   "abc",
		"tls":       map[string]any{"key_file": "/etc/x.key", "min_version": "1.2"},
		"endpoints": []any{"https://a:b@c/1", "http://d/2"},
		"keep":      "visible",
	}
	m, ok := maskAny(in).(map[string]any)
	if !ok {
		t.Fatal("maskAny 应返回 map")
	}
	if m["password"] != Masked || m["api_key"] != Masked {
		t.Error("键名含 password/api_key 的字段必须整体脱敏")
	}
	tls := m["tls"].(map[string]any)
	if tls["key_file"] != Masked || tls["min_version"] != "1.2" {
		t.Errorf("嵌套脱敏错误: %v", tls)
	}
	eps := m["endpoints"].([]any)
	if eps[0] != "https://***@c/1" || eps[1] != "http://d/2" {
		t.Errorf("数组内 URL 脱敏错误: %v", eps)
	}
	if m["keep"] != "visible" {
		t.Error("正常字段不应被改动")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
