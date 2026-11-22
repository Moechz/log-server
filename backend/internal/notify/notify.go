// Package notify 实现告警通知通道（FR-803）：
//
//	smtp    —— 自配 SMTP 邮件（STARTTLS / 隐式 TLS / 明文）
//	webhook —— HTTP POST JSON（Loki/Alertmanager/自建端点等）
//
// ★ TOS 通知中心（`ter_msg_add`）**有意未接入**：向 TOS 注入消息的官方通道/方法尚未提供
//
//	（平台需求 R8 / 开放问题 Q8）。在拿到官方方法前，应用不向平台写消息、不注册 msgType（D-017）。
//
// 约束：只用标准库（D-015 / 零外部依赖）；凭据只落在应用私有配置文件，绝不进日志/argv/诊断包。
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Config 是单个通道的配置。
type Config struct {
	Kind        string `json:"kind"` // tos | smtp | webhook
	Name        string `json:"name,omitempty"`
	Enabled     bool   `json:"enabled"`
	MinSeverity int    `json:"min_severity"` // 仅推送 severity <= 该值（0=全部，7=最低）

	// SMTP
	SMTPHost string   `json:"smtp_host,omitempty"`
	SMTPPort int      `json:"smtp_port,omitempty"`
	SMTPUser string   `json:"smtp_user,omitempty"`
	SMTPPass string   `json:"smtp_pass,omitempty"`
	SMTPFrom string   `json:"smtp_from,omitempty"`
	SMTPTo   []string `json:"smtp_to,omitempty"`
	SMTPTLS  string   `json:"smtp_tls,omitempty"` // starttls(默认) | tls | none

	// Webhook
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Message 是一条待通知的告警（与 alert.Alert 解耦）。
type Message struct {
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Level     int       `json:"level"` // syslog severity 0-7
	LevelText string    `json:"level_text"`
	Category  string    `json:"category"`
	Host      string    `json:"host"`
	App       string    `json:"app"`
	RuleID    string    `json:"rule_id"`
	At        time.Time `json:"at"`
}

// Delivery 是一次投递结果（用于 UI 展示最近投递）。
type Delivery struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	Title  string    `json:"title,omitempty"`
	Target string    `json:"target,omitempty"`
}

type channel interface {
	Kind() string
	Target() string
	Send(ctx context.Context, m Message) error
}

// Dispatcher 管理全部通道并分发通知。
type Dispatcher struct {
	mu       sync.Mutex
	cfgs     map[string]Config // kind → 最近一次配置（用于 Test）
	channels []channel
	recent   []Delivery
	log      *slog.Logger
}

// New 创建 Dispatcher。
func New(lg *slog.Logger) *Dispatcher {
	if lg == nil {
		lg = slog.Default()
	}
	return &Dispatcher{cfgs: map[string]Config{}, log: lg}
}

// ValidKinds 返回支持的通道类型。
func ValidKinds() []string { return []string{"smtp", "webhook"} }

// Apply 按配置重建通道（幂等）。返回配置告警（如端口非法）。
func (d *Dispatcher) Apply(cfgs []Config) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfgs = map[string]Config{}
	d.channels = nil
	var warns []string
	for _, c := range cfgs {
		k := strings.ToLower(strings.TrimSpace(c.Kind))
		c.Kind = k
		d.cfgs[k] = c
		if !c.Enabled {
			continue
		}
		ch, err := build(k, c)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		d.channels = append(d.channels, ch)
	}
	return warns
}

// Send 向所有通道分发（每个通道独立判断级别过滤与错误，互不影响）。
func (d *Dispatcher) Send(m Message) {
	d.mu.Lock()
	chans := append([]channel(nil), d.channels...)
	cfgs := map[string]Config{}
	for k, v := range d.cfgs {
		cfgs[k] = v
	}
	d.mu.Unlock()

	for _, ch := range chans {
		cfg := cfgs[ch.Kind()]
		if !passLevel(cfg.MinSeverity, m.Level) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := ch.Send(ctx, m)
		cancel()
		res := Delivery{Kind: ch.Kind(), At: time.Now(), OK: err == nil, Title: m.Title, Target: ch.Target()}
		if err != nil {
			res.Error = err.Error()
			d.log.Warn("通知投递失败", "kind", ch.Kind(), "err", err)
		}
		d.record(res)
	}
}

// Test 用当前配置对某通道发一条测试消息。
func (d *Dispatcher) Test(ctx context.Context, kind string) error {
	d.mu.Lock()
	cfg, ok := d.cfgs[strings.ToLower(strings.TrimSpace(kind))]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("未配置该通道：%s", kind)
	}
	cfg.Enabled = true // 测试时即使未启用也发
	ch, err := build(cfg.Kind, cfg)
	if err != nil {
		return err
	}
	m := Message{
		Title: "[log server for TOS] 通知通道测试", Level: 6, LevelText: "info", Category: "应用",
		Host: "本机", At: time.Now(),
		Body: "这是一条来自 log server for TOS 的测试消息；收到即表示该通道配置可用。",
	}
	err = ch.Send(ctx, m)
	res := Delivery{Kind: ch.Kind(), At: time.Now(), OK: err == nil, Title: m.Title, Target: ch.Target()}
	if err != nil {
		res.Error = err.Error()
	}
	d.record(res)
	return err
}

// Recent 返回最近投递（新的在前）。
func (d *Dispatcher) Recent(n int) []Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n <= 0 || n > len(d.recent) {
		n = len(d.recent)
	}
	out := make([]Delivery, 0, n)
	for i := len(d.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, d.recent[i])
	}
	return out
}

func (d *Dispatcher) record(res Delivery) {
	d.mu.Lock()
	d.recent = append(d.recent, res)
	if len(d.recent) > 200 {
		d.recent = d.recent[len(d.recent)-200:]
	}
	d.mu.Unlock()
}

// passLevel：MinSeverity 0 表示不过滤；否则只推 severity 数值 <= 阈值（越小越严重）的消息。
func passLevel(min int, level int) bool {
	if min <= 0 {
		return true
	}
	return level <= min
}

func build(kind string, c Config) (channel, error) {
	switch kind {
	case "smtp":
		if strings.TrimSpace(c.SMTPHost) == "" || len(c.SMTPTo) == 0 {
			return nil, fmt.Errorf("SMTP 需要 host 与收件人")
		}
		port := c.SMTPPort
		if port == 0 {
			port = 587
		}
		return &smtpChannel{cfg: c, port: port}, nil
	case "webhook":
		if strings.TrimSpace(c.URL) == "" {
			return nil, fmt.Errorf("Webhook 需要 URL")
		}
		return &webhookChannel{cfg: c, hc: &http.Client{Timeout: 15 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("未知通道类型 %q", kind)
	}
}

// ── SMTP ────────────────────────────────────────────────────────────

type smtpChannel struct {
	cfg  Config
	port int
}

func (c *smtpChannel) Kind() string { return "smtp" }
func (c *smtpChannel) Target() string {
	return fmt.Sprintf("%s:%d → %s", c.cfg.SMTPHost, c.port, strings.Join(c.cfg.SMTPTo, ","))
}

func (c *smtpChannel) Send(ctx context.Context, m Message) error {
	host := c.cfg.SMTPHost
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", c.port))
	from := c.cfg.SMTPFrom
	if from == "" {
		from = c.cfg.SMTPUser
	}
	if from == "" {
		from = "log-server-for-tos@localhost"
	}

	var client *smtp.Client
	mode := strings.ToLower(c.cfg.SMTPTLS)
	if mode == "" {
		mode = "starttls"
	}
	if mode == "tls" {
		d := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return fmt.Errorf("TLS 连接失败: %w", err)
		}
		client, err = smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
	} else {
		d := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("连接失败: %w", err)
		}
		client, err = smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		if mode == "starttls" {
			if ok, _ := client.Extension("STARTTLS"); ok {
				if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
					return fmt.Errorf("STARTTLS 失败: %w", err)
				}
			}
		}
	}
	defer client.Close()

	if c.cfg.SMTPUser != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(smtp.PlainAuth("", c.cfg.SMTPUser, c.cfg.SMTPPass, host)); err != nil {
				return fmt.Errorf("SMTP 鉴权失败: %w", err)
			}
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, to := range c.cfg.SMTPTo {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("收件人 %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildEmail(from, c.cfg.SMTPTo, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func buildEmail(from string, to []string, m Message) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mimeHeader(m.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(m.Body)
	if m.Host != "" || m.App != "" {
		fmt.Fprintf(&b, "\r\n\r\n主机: %s  应用: %s", m.Host, m.App)
	}
	fmt.Fprintf(&b, "\r\n时间: %s\r\n", m.At.Format(time.RFC3339))
	return b.Bytes()
}

// mimeHeader 简单防头注入（去掉 CR/LF）。
func mimeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// ── Webhook ─────────────────────────────────────────────────────────

type webhookChannel struct {
	cfg Config
	hc  *http.Client
}

func (c *webhookChannel) Kind() string   { return "webhook" }
func (c *webhookChannel) Target() string { return c.cfg.URL }

func (c *webhookChannel) Send(ctx context.Context, m Message) error {
	method := strings.ToUpper(c.cfg.Method)
	if method == "" {
		method = http.MethodPost
	}
	body, _ := json.Marshal(map[string]any{
		"source":     "log-server-for-tos",
		"title":      m.Title,
		"body":       m.Body,
		"level":      m.Level,
		"level_text": m.LevelText,
		"category":   m.Category,
		"host":       m.Host,
		"app":        m.App,
		"rule_id":    m.RuleID,
		"at":         m.At.UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
