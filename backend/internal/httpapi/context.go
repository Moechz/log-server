package httpapi

import "context"

// contextTODO 提供一个短超时上下文给「非请求路径」的内部调用（如总览查询）。
// 单独成文件是为了让 server.go 的意图更清楚，也便于将来替换。
func contextTODO() context.Context {
	return context.Background()
}
