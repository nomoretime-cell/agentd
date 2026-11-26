package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

type IMRoute struct {
	NotifyUser       string `json:"notify_user,omitempty"`
	BotID            string `json:"bot_id,omitempty"`
	ConversationID   string `json:"conversation_id"`
	ConversationType string `json:"conversation_type"`
	Sender           string `json:"sender"`
	Mentioned        bool   `json:"mentioned"`
	Webhook          string `json:"session_webhook"`
	ExpiresAt        int64  `json:"expires_at"`
}
type imTargetKey struct{}

// imChannel is the one IM platform this process uses. Core modules call it
// without knowing which platform it is.
type imChannel interface {
	// deliver sends one notification and interprets the platform receipt as
	// sent, pending (retry later), offline (not connected), failed or unknown.
	deliver(ctx context.Context, n Notification) string
	// owner returns the target for proactive messages to the owner and
	// whether the platform is configured to send them.
	owner() (target string, enabled bool)
	// run keeps the platform connection open until ctx ends.
	run(ctx context.Context, first *websocket.Conn)
	connect(context.Context) (*websocket.Conn, error)
	// connected reports whether the platform connection is open now.
	connected() bool
}

type imRuntime struct {
	cfg     *Config
	channel imChannel
}

func (s *Service) imConfig() *Config { return s.im.Load().cfg }

func newIMChannel(s *Service, cfg *Config) imChannel {
	if cfg.provider() == "none" {
		return disabledIM{}
	}
	if cfg.provider() == "wecom" {
		return &weComChannel{s: s, cfg: &cfg.WeCom, url: "wss://openws.work.weixin.qq.com"}
	}
	return &dingTalkChannel{s: s, cfg: &cfg.DingTalk}
}

// imRoutes is the part of an Inbox payload that records where an IM message
// came from. The JSON field names are part of the stored data format.
type imRoutes struct {
	WeCom    *IMRoute `json:"wecom,omitempty"`
	DingTalk *IMRoute `json:"dingtalk,omitempty"`
}

func (r imRoutes) fromIM() bool { return r.DingTalk != nil || r.WeCom != nil }

// startIM and stopIMLoop own a single connection loop. stop waits for callbacks
// to finish before the active identity can change.
func (s *Service) startIM(first *websocket.Conn) {
	ctx, cancel := context.WithCancel(s.imContext)
	done := make(chan struct{})
	s.stopIM = func() { cancel(); <-done }
	channel := s.im.Load().channel
	go func() {
		defer close(done)
		if first != nil {
			defer first.Close()
		}
		channel.run(ctx, first)
	}()
}
func (s *Service) stopIMLoop() {
	if s.stopIM != nil {
		s.stopIM()
		s.stopIM = nil
	}
}
func (s *Service) teamPrompt() string {
	return "你是群内团队助手。结合团队资料和当前可用工具回答问题。需要实时信息时使用可用的联网搜索并注明来源，没有依据时明确说明。团队资料：\n" + s.imConfig().WeCom.TeamKnowledge
}

// Credentials replaced during this process can still occur in historical text.
func (s *Service) rememberIMSecrets(cfg Config) {
	var values []string
	if old := s.imSecrets.Load(); old != nil {
		values = slices.Clone(*old)
	}
	for _, secret := range []string{cfg.DingTalk.ClientSecret, cfg.WeCom.Secret} {
		if secret != "" && !slices.Contains(values, secret) {
			values = append(values, secret)
		}
	}
	s.imSecrets.Store(&values)
}

// redactIM removes IM credentials and session webhooks from text leaving the host.
func (s *Service) redactIM(v string, mask func(value, label string) string) string {
	v = dingTalkSecretField.ReplaceAllStringFunc(v, func(field string) string {
		prefix := dingTalkSecretField.FindStringSubmatch(field)[1]
		return prefix + mask(field[len(prefix):len(field)-1], "[redacted]") + `"`
	})
	v = dingTalkSecretURL.ReplaceAllStringFunc(v, func(url string) string { return mask(url, "[dingtalk-webhook-redacted]") })
	for _, secret := range *s.imSecrets.Load() {
		for _, spelling := range secretSpellings(secret) {
			v = strings.ReplaceAll(v, spelling, mask(spelling, "[redacted]"))
		}
	}
	return v
}

// imEventKind reports whether an Inbox payload kind is a message received
// from an IM provider. Such events use the provider name as their kind.
func imEventKind(kind string) bool { return kind == "dingtalk" || kind == "wecom" }

func imReplyTarget(target string) bool {
	return strings.HasPrefix(target, "dingtalk:") || strings.HasPrefix(target, "wecom:")
}

func imOwnerTarget(target string) bool {
	return strings.HasPrefix(target, "dingtalk-owner:") || strings.HasPrefix(target, "wecom-owner:")
}

type disabledIM struct{}

func (disabledIM) connect(context.Context) (*websocket.Conn, error) { return nil, nil }
func (disabledIM) run(context.Context, *websocket.Conn)             {}
func (disabledIM) connected() bool                                  { return false }
func (disabledIM) owner() (string, bool)                            { return "", false }
func (disabledIM) deliver(context.Context, Notification) string     { return "offline" }

// Disabled providers keep their work in the ordinary queues for a later restart.
func (c Config) parkedProviders() (string, string) {
	switch c.selectedIM() {
	case "wecom":
		return "dingtalk", "dingtalk"
	case "none":
		return "wecom", "dingtalk"
	default:
		return "wecom", "wecom"
	}
}

func (s *Service) ownerTarget() string { t, _ := s.im.Load().channel.owner(); return t }

func (s *Service) canMessageOwner() bool {
	_, enabled := s.im.Load().channel.owner()
	return enabled && s.cfg.Permissions["send_message"] == "allow"
}

func dingTalkTarget(id int64) string { return "dingtalk:" + strconv.FormatInt(id, 10) }
func (c Config) provider() string {
	if c.IM.Provider == "" {
		return "dingtalk"
	}
	return c.IM.Provider
}
func (p Payload) imTarget(id int64) string {
	if p.WeCom != nil {
		return "wecom:" + strconv.FormatInt(id, 10)
	}
	if p.DingTalk != nil {
		return dingTalkTarget(id)
	}
	return ""
}
func (r *IMRoute) team() bool { return r != nil && r.BotID != "" && r.ConversationType == "group" }

// group reports whether the reply is visible to other members of the
// conversation, on any channel ("2" is a DingTalk group, "group" a WeCom group).
func (r *IMRoute) group() bool {
	return r != nil && (r.ConversationType == "2" || r.ConversationType == "group")
}

// imScope names the principal and conversation of an IM run. A group is one
// principal whoever speaks in it; a private chat belongs to its sender.
func (s *Service) imScope(r *IMRoute) runScope {
	cfg := s.imConfig()
	provider := cfg.provider()
	sc := runScope{Role: defaultRole, Conversation: stableID(provider + "\x00conversation\x00" + r.ConversationID)}
	owner := cfg.DingTalk.AllowedSender
	if provider == "wecom" {
		owner = cfg.WeCom.Owner
	}
	switch {
	case r.group():
		sc.Principal = "group-" + stableID(provider+"\x00group\x00"+r.ConversationID)
	case r.Sender != "" && r.Sender == owner:
		sc.Principal = "owner"
	default:
		sc.Principal = "user-" + stableID(provider+"\x00user\x00"+r.Sender)
	}
	return sc
}

type sharedTurnKey struct{}

// isSharedTurn reports whether this run's replies are visible to people other than the owner.
func isSharedTurn(ctx context.Context) bool { v, _ := ctx.Value(sharedTurnKey{}).(bool); return v }

type teamTurnKey struct{}

func isTeamTurn(ctx context.Context) bool { v, _ := ctx.Value(teamTurnKey{}).(bool); return v }

type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) imRoute(ctx context.Context, q rowQuery, target string) (*IMRoute, error) {
	provider, rawID, _ := strings.Cut(target, ":")
	if provider != s.imConfig().provider() {
		return nil, ErrDenied
	}
	id, e := strconv.ParseInt(rawID, 10, 64)
	if e != nil || id <= 0 {
		return nil, ErrDenied
	}
	var raw string
	if e = q.QueryRowContext(ctx, "SELECT payload FROM inbox WHERE id=?", id).Scan(&raw); e != nil {
		return nil, e
	}
	var p Payload
	if json.Unmarshal([]byte(raw), &p) != nil || p.imTarget(id) != target {
		return nil, ErrDenied
	}
	if !s.permitsIM(p) {
		return nil, ErrDenied
	}
	if p.WeCom != nil {
		return p.WeCom, nil
	}
	if !validDingTalkWebhook(p.DingTalk.Webhook) {
		return nil, ErrDenied
	}
	return p.DingTalk, nil
}
func (s *Service) replyTarget(ctx context.Context, ev Event) (string, error) {
	if target := ev.Payload.imTarget(ev.ID); target != "" {
		return target, nil
	}
	if ev.Payload.Kind != "approval" {
		return "", nil
	}
	var key string
	if e := s.read.QueryRowContext(ctx, "SELECT dedup_key FROM actions WHERE id=?", ev.Payload.ActionID).Scan(&key); e != nil {
		return "", e
	}
	parts := strings.Split(key, ":")
	if len(parts) < 3 || parts[0] != "ev" {
		return "", nil
	}
	id, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil {
		return "", e
	}
	var raw string
	if e = s.read.QueryRowContext(ctx, "SELECT payload FROM inbox WHERE id=?", id).Scan(&raw); e != nil {
		return "", e
	}
	var p Payload
	if e = json.Unmarshal([]byte(raw), &p); e != nil {
		return "", e
	}
	return p.imTarget(id), nil
}

func (s *Service) permitsIM(p Payload) bool {
	cfg := s.imConfig()
	if r := p.DingTalk; r != nil && cfg.provider() == "dingtalk" {
		return cfg.DingTalk.permits(r.Sender, r.ConversationID, r.ConversationType, r.Mentioned)
	}
	return p.WeCom != nil && cfg.provider() == "wecom" && cfg.WeCom.permits(p.WeCom)
}
func (s *Service) progressFor(p Payload) bool {
	if !s.permitsIM(p) {
		return false
	}
	if p.WeCom != nil {
		return s.imConfig().WeCom.ProgressMessages
	}
	return s.imConfig().DingTalk.ProgressMessages
}

func (s *Service) restrictTeamTools(ctx context.Context) bool {
	return isTeamTurn(ctx) && !s.imConfig().WeCom.GroupOwnerTools
}

func (s *Service) turnTools(ctx context.Context) []Tool {
	if s.restrictTeamTools(ctx) {
		return nil
	}
	return toolDefinitions()
}

func appendIMQuote(body, quoted, channel string, maxBytes int) string {
	if strings.TrimSpace(quoted) == "" {
		quoted = "（未提供可读原文。如需原文，请用户粘贴文字或链接。）"
	}
	prefix := "\n\n[用户引用的" + channel + "消息，作为参考资料]\n"
	budget := maxBytes - len(body) - len(prefix)
	if budget < len("…") {
		return body
	}
	return body + prefix + clipHeadline(quoted, budget)
}

const imTextLimit = 4000

func imText(v string) string {
	// One bounded text message; splitting creates additional ambiguous side effects.
	if len(v) > imTextLimit {
		return strings.ToValidUTF8(v[:3900], "") + "\n[结果过长，已截断；完整内容请在本机查看执行记录。]"
	}
	return v
}

func (s *Service) addIMAnswer(out *Outcome, eventID int64, answer string) {
	if out.ReplyTarget == "" || len(out.Notifications) > 0 || s.cfg.Permissions["send_message"] != "allow" {
		return
	}
	if strings.TrimSpace(answer) == "" {
		answer = "本次处理已结束，模型未返回可展示的文本；请在本机查看执行记录。"
	}
	answer = s.redact(answer)
	if len(answer) > s.cfg.Limits.MaxOutputBytes {
		answer = strings.ToValidUTF8(answer[:s.cfg.Limits.MaxOutputBytes], "")
	}
	out.Notifications = append(out.Notifications, Notification{Key: fmt.Sprintf("reply:%d", eventID), Target: out.ReplyTarget, Content: answer})
}

// runChannel records the originating conversation channel, independently of the current configuration.
func runChannel(ev Event, target string) string {
	if ev.Payload.WebChat != "" {
		return "web"
	}
	if target == "" {
		target = ev.Payload.imTarget(ev.ID)
	}
	if provider, _, ok := strings.Cut(target, ":"); ok && imReplyTarget(target) {
		return provider
	}
	return "local"
}
