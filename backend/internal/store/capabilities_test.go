package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCapabilitiesJSONSnakeCase 锁住对外 JSON 字段名（前端按 snake_case 读取）。
// 曾经缺 tag → 序列化为 FullTextSearch，前端读 full_text_search 取不到，能力卡全部显示“不可用”。
func TestCapabilitiesJSONSnakeCase(t *testing.T) {
	b, err := json.Marshal(Capabilities{
		Engine: "victorialogs", FullTextSearch: true, RegexSearch: true,
		FieldSearch: true, NativeTTL: true, MaxRetentionDays: 3650,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"engine":"victorialogs"`,
		`"full_text_search":true`,
		`"regex_search":true`,
		`"field_search":true`,
		`"native_ttl":true`,
		`"native_aggregate":false`,
		`"sql":false`,
		`"max_retention_days":3650`,
		`"per_profile_retention":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("JSON 缺少 %s：%s", want, s)
		}
	}
}
