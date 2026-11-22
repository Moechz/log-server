// Package store 是**存储适配层**（FR-312）。
//
// 硬约束：业务层只依赖本接口，**不得出现 VictoriaLogs 的查询语法或 HTTP 细节**；
// 换引擎时只新增一个实现包（见 TECH-DESIGN §5.4）。
package store

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// ErrUnsupported 表示当前引擎未实现该能力（上层据此降级）。
var ErrUnsupported = errors.New("store: 该能力在当前引擎上未实现")

// TimeRange 是查询时间范围（UTC）。
type TimeRange struct{ From, To time.Time }

// Query 是检索条件。
type Query struct {
	Range           TimeRange
	Classes         []string
	SourceKinds     []string
	Hosts           []string
	Apps            []string
	MinSeverity     *int
	Text            string
	IsAudit         *bool
	OnlyTrustedTime bool
	Limit           int
	Cursor          string // 游标分页（C4；VL 实现见 vlstore 说明）
}

// Result 是检索结果。
type Result struct {
	Logs       []map[string]any
	NextCursor string
	TookMS     int64
	Truncated  bool
}

// BucketCount 是直方图的桶。
type BucketCount struct {
	Bucket time.Time
	Count  int64
}

// Usage 是容量与保留统计（FR-305 / FR-311）。
type Usage struct {
	TotalBytes     uint64
	RecordCount    uint64
	OldestEvent    time.Time
	NewestEvent    time.Time
	DiskTotalBytes uint64
	DiskFreeBytes  uint64
	WatermarkPct   float64
}

// RetentionProfile 是保留策略档案（FR-310）。
type RetentionProfile struct {
	Name   string
	Days   int
	Locked bool
}

// Capabilities 是引擎自述能力（上层据此做特性降级）。
//
// ★ JSON tag 使用 snake_case：前端（storage 页）按这些名字读取，
//
//	曾经因为缺 tag 而序列化为驼峰，导致“存储引擎能力”卡永远显示不可用。
type Capabilities struct {
	Engine           string `json:"engine"`
	FullTextSearch   bool   `json:"full_text_search"`
	RegexSearch      bool   `json:"regex_search"`
	FieldSearch      bool   `json:"field_search"`
	NativeTTL        bool   `json:"native_ttl"`
	NativeAggregate  bool   `json:"native_aggregate"`
	SQL              bool   `json:"sql"`
	MaxRetentionDays int    `json:"max_retention_days"`
	// PerProfileRetention 引擎是否支持"按分类/档案分别保留"。
	// VictoriaLogs 当前不支持（只有全局 -retentionPeriod + 磁盘上限）→ 阶段一只承诺全局保留。
	PerProfileRetention bool `json:"per_profile_retention"`
}

// Store 是存储适配层接口。实现必须并发安全。
type Store interface {
	Open(ctx context.Context, cfg Config) error
	Ping(ctx context.Context) error
	Close() error
	Capabilities() Capabilities

	// Write 至少一次投递；返回错误时上层不 ack（队列重试）。
	Write(ctx context.Context, batch []*model.Record) error

	// Query 检索；Limit 硬上限 1000；应支持游标而非大 OFFSET。
	Query(ctx context.Context, q Query) (*Result, error)

	// Histogram 时间轴直方图。
	Histogram(ctx context.Context, q Query, interval time.Duration) ([]BucketCount, error)

	// Usage 必须廉价（可内部缓存），供水位与容量预估使用。
	Usage(ctx context.Context) (Usage, error)

	// EnsureProfile 声明保留档案（引擎不支持时返回 ErrUnsupported）。
	EnsureProfile(ctx context.Context, p RetentionProfile) error
}

// Config 是打开存储的配置。
type Config struct {
	Engine      string // victorialogs | clickhouse
	Addr        string // 如 127.0.0.1:19428
	StoragePath string // 仅内置引擎使用
	Database    string
	Timeout     time.Duration
	LowResource bool
}

// ExportFormat 是导出格式（FR-404）。
type ExportFormat string

const (
	ExportRawLines ExportFormat = "raw"
	ExportJSON     ExportFormat = "json"
	ExportCSV      ExportFormat = "csv"
)

// Exporter 是可选的导出能力（引擎不支持时上层降级为分页拉取）。
type Exporter interface {
	ExportRaw(ctx context.Context, q Query, w io.Writer, format ExportFormat) (int64, error)
}

// ContextResult 是「上下文」查询结果（FR-406）：pivot 前后的同源日志，均按时间正序。
type ContextResult struct {
	Before []map[string]any
	After  []map[string]any
}

// ContextReader 是可选的「按时间点取前后 N 条上下文」能力（FR-406）。
//
// 与 VL 官方 UI 同口径：before 用 `sort by (_time) desc | limit N`（取最靠近 pivot 的前 N 条），
// after 用 `sort by (_time) asc | limit N`（取 pivot 之后最早的 N 条）。
type ContextReader interface {
	Context(ctx context.Context, q Query, pivot time.Time, before, after int) (ContextResult, error)
}
