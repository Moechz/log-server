package httpapi

import (
	"net/http"
	"testing"
)

func TestStorageLocationGet(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "GET", "/api/v1/storage/location", "")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("GET location 失败: %d %v", code, body)
	}
	d, _ := body["data"].(map[string]any)
	if d == nil || d["engine_dir"] == "" {
		t.Fatalf("缺少 engine_dir: %v", body)
	}
	if d["policy"] == nil {
		t.Fatalf("缺少 policy: %v", body)
	}
}

func TestStorageBrowseRejectsSystemPath(t *testing.T) {
	s := newTestServer(t)
	code, _ := do(t, s, "GET", "/api/v1/storage/browse?path=/etc", "")
	if code != http.StatusBadRequest {
		t.Fatalf("浏览 /etc 应被拒绝，得到 %d", code)
	}
}

func TestStorageLocationPutRejectsSystemPath(t *testing.T) {
	s := newTestServer(t)
	code, _ := do(t, s, "PUT", "/api/v1/storage/location", `{"engine_dir":"/var/lib/log-server-for-tos/evil"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("保存系统分区路径应被拒绝，得到 %d", code)
	}
}
