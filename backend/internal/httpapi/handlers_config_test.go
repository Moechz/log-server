package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Moechz/log-server/backend/internal/config"
)

// newConfigTestServer 构造一个仅启用配置备份接口的 Server（不启用鉴权）。
func newConfigTestServer(t *testing.T, dataDir string) *Server {
	t.Helper()
	cfg := config.Config{DataDir: dataDir, ConfigDir: filepath.Join(dataDir, "config")}
	if err := os.MkdirAll(cfg.ConfigDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return New(cfg, nil, nil, nil, nil, "", nil)
}

func doConfig(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// TestConfigExportImport_RoundTrip 覆盖 FR-1002 的主路径：
// 导出 → 清空 → 导入 → 内容逐字节一致，且提示需重启生效。
func TestConfigExportImport_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newConfigTestServer(t, dir)

	appCfg := `{"sources":[{"path":"/var/log/syslog","enabled":true}],"forward_targets":[{"id":"t1","addr":"10.0.0.9:514"}]}`
	storage := `{"engine_dir":"/Volume2/apps/log-server-for-tos/data/engine"}`
	for name, body := range map[string]string{"config.json": appCfg, "storage.json": storage} {
		if err := os.WriteFile(filepath.Join(s.configDir(), name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	w := doConfig(t, s, "GET", "/api/v1/config/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "log-server-for-tos-config-") {
		t.Fatalf("导出应带下载文件名，实际 %q", cd)
	}
	var got configBackup
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("导出件不是合法 JSON: %v", err)
	}
	if got.AppID != "log-server-for-tos" || got.Version == "" || got.ExportedAt == "" {
		t.Fatalf("导出件缺少元信息: %+v", got)
	}
	if len(got.Files) != 2 {
		t.Fatalf("应导出 2 个文件，实际 %d", len(got.Files))
	}

	// 清空后导入
	if err := os.Remove(filepath.Join(s.configDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.configDir(), "storage.json")); err != nil {
		t.Fatal(err)
	}
	w = doConfig(t, s, "POST", "/api/v1/config/import", w.Body.String())
	if w.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Written         []string `json:"written"`
			RestartRequired bool     `json:"restart_required"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Written) != 2 || !resp.Data.RestartRequired {
		t.Fatalf("导入响应异常: %+v", resp.Data)
	}
	for name, want := range map[string]string{"config.json": appCfg, "storage.json": storage} {
		b, err := os.ReadFile(filepath.Join(s.configDir(), name))
		if err != nil {
			t.Fatalf("%s 未写回: %v", name, err)
		}
		// 导出件是格式化后的 JSON → 按语义比较（键值一致即可）
		var got, exp any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", name, err)
		}
		if err := json.Unmarshal([]byte(want), &exp); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, exp) {
			t.Fatalf("%s 语义不一致: %s", name, b)
		}
	}
}

// TestConfigImport_RejectsBadInput 覆盖安全与健壮性边界：
// 路径穿越、非白名单文件、非法 JSON、外来 app_id、空备份件。
func TestConfigImport_RejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	s := newConfigTestServer(t, dir)
	outside := filepath.Join(dir, "..", "escaped.json")

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"路径穿越", `{"files":{"../../escaped.json":{}}}` + "\n", http.StatusBadRequest, "unknown_file"},
		{"绝对路径", `{"files":{"/etc/passwd":{}}}` + "\n", http.StatusBadRequest, "unknown_file"},
		{"非白名单名", `{"files":{"other.json":{}}}` + "\n", http.StatusBadRequest, "unknown_file"},
		{"非法 JSON 值", `{"files":{"config.json":"not-json"}}` + "\n", http.StatusBadRequest, "invalid_json"},
		{"外来 app_id", `{"app_id":"someone-else","files":{"config.json":{}}}` + "\n", http.StatusBadRequest, "app_id_mismatch"},
		{"空备份件", `{"files":{}}` + "\n", http.StatusBadRequest, "empty_backup"},
		{"非 JSON 体", `not-json-at-all`, http.StatusBadRequest, "bad_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doConfig(t, s, "POST", "/api/v1/config/import", tc.body)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			var resp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Error.Code != tc.code {
				t.Fatalf("error code=%q want=%q body=%s", resp.Error.Code, tc.code, w.Body.String())
			}
		})
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("路径穿越防护失效：%s 被创建", outside)
	}
	// 目录里不得出现 .tmp 残留
	entries, _ := os.ReadDir(s.configDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("原子写留下临时文件: %s", e.Name())
		}
	}
}

// TestConfigExport_EmptyAndInvalid：未配置过任何文件时导出仍成功（files 为空对象），
// 但存在非法 JSON 的配置文件时拒绝导出（否则会生成无法导入的备份件）。
func TestConfigExport_EmptyAndInvalid(t *testing.T) {
	dir := t.TempDir()
	s := newConfigTestServer(t, dir)

	w := doConfig(t, s, "GET", "/api/v1/config/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("空配置导出应成功，实际 %d", w.Code)
	}
	var got configBackup
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("空配置应导出 0 个文件，实际 %d", len(got.Files))
	}

	if err := os.WriteFile(filepath.Join(s.configDir(), "config.json"), []byte("{broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	w = doConfig(t, s, "GET", "/api/v1/config/export", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("非法配置文件应拒绝导出，实际 %d", w.Code)
	}
}
