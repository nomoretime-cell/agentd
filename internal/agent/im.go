package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	run(ctx context.Context)
	// connected reports whether the platform connection is open now.
	connected() bool
}

func newIMChannel(s *Service) imChannel {
	if s.cfg.provider() == "wecom" {
		return &weComChannel{s: s, url: "wss://openws.work.weixin.qq.com"}
	}
	return &dingTalkChannel{s: s}
}

// imRoutes is the part of an Inbox payload that records where an IM message
// came from. The JSON field names are part of the stored data format.
type imRoutes struct {
	WeCom    *IMRoute `json:"wecom,omitempty"`
	DingTalk *IMRoute `json:"dingtalk,omitempty"`
}

func (r imRoutes) fromIM() bool { return r.DingTalk != nil || r.WeCom != nil }

func (s *Service) teamPrompt() string {
	return "你是群内团队助手。结合团队资料和当前可用工具回答问题。需要实时信息时使用可用的联网搜索并注明来源，没有依据时明确说明。团队资料：\n" + s.cfg.WeCom.TeamKnowledge
}

// redactIM removes IM credentials and session webhooks from text leaving the host.
func (s *Service) redactIM(v string, mask func(value, label string) string) string {
	v = dingTalkSecretField.ReplaceAllStringFunc(v, func(field string) string {
		prefix := dingTalkSecretField.FindStringSubmatch(field)[1]
		return prefix + mask(field[len(prefix):len(field)-1], "[redacted]") + `"`
	})
	v = dingTalkSecretURL.ReplaceAllStringFunc(v, func(url string) string { return mask(url, "[dingtalk-webhook-redacted]") })
	for _, secret := range []string{s.cfg.DingTalk.ClientSecret, s.cfg.WeCom.Secret} {
		if secret != "" {
			v = strings.ReplaceAll(v, secret, mask(secret, "[redacted]"))
		}
	}
	return v
}

func imReplyTarget(target string) bool {
	return strings.HasPrefix(target, "dingtalk:") || strings.HasPrefix(target, "wecom:")
}

func imOwnerTarget(target string) bool {
	return strings.HasPrefix(target, "dingtalk-owner:") || strings.HasPrefix(target, "wecom-owner:")
}

// IM events of the provider that is not selected stay queued until it is
// selected again. Their kind is the provider name.
func (c Config) parkedEventKind() string { return c.inactiveProvider() }

// Targets of the provider that is not selected stay queued until it is selected again.
func (c Config) parkedTargetGlobs() (reply, owner string) {
	return c.inactiveProvider() + ":*", c.inactiveProvider() + "-owner:*"
}

func (s *Service) ownerTarget() string { t, _ := s.im.owner(); return t }

func (s *Service) canMessageOwner() bool {
	_, enabled := s.im.owner()
	return enabled && s.cfg.Permissions["send_message"] == "allow"
}

func dingTalkTarget(id int64) string { return "dingtalk:" + strconv.FormatInt(id, 10) }
func (c Config) provider() string {
	if c.IM.Provider == "" {
		return "dingtalk"
	}
	return c.IM.Provider
}
func (c Config) inactiveProvider() string {
	if c.provider() == "wecom" {
		return "dingtalk"
	}
	return "wecom"
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
	if provider != s.cfg.provider() {
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
	if r := p.DingTalk; r != nil && s.cfg.provider() == "dingtalk" {
		return s.cfg.DingTalk.permits(r.Sender, r.ConversationID, r.ConversationType, r.Mentioned)
	}
	return p.WeCom != nil && s.cfg.provider() == "wecom" && s.cfg.WeCom.permits(p.WeCom)
}
func (s *Service) progressFor(p Payload) bool {
	if !s.permitsIM(p) {
		return false
	}
	if p.WeCom != nil {
		return s.cfg.WeCom.ProgressMessages
	}
	return s.cfg.DingTalk.ProgressMessages
}

func (s *Service) restrictTeamTools(ctx context.Context) bool {
	return isTeamTurn(ctx) && !s.cfg.WeCom.GroupOwnerTools
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
