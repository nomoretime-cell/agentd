package agent

// DingTalk Stream is ingress only. Replies use the callback's expiring session
// webhook, kept in the host-owned Inbox payload, never exposed to the model.
import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"

	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const dingTalkTopic = "/v1.0/im/bot/messages/get"

var dingTalkSecretField = regexp.MustCompile(`("session_webhook"\s*:\s*")[^"]*"`)
var dingTalkSecretURL = regexp.MustCompile(`https://oapi\.dingtalk\.com/robot/sendBySession[^\s"<>\\]*`)

type DingTalkConfig struct {
	AllowAllGroups       bool     `yaml:"allow_all_groups"`
	ProgressMessages     bool     `yaml:"progress_messages"`
	Enabled              bool     `yaml:"enabled"`
	ClientID             string   `yaml:"client_id"`
	ClientSecret         string   `yaml:"client_secret"`
	DiscoveryOnly        bool     `yaml:"discovery_only"`
	AllowedSender        string   `yaml:"allowed_sender"`
	AllowedConversations []string `yaml:"allowed_conversations"`
}

func (c DingTalkConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("dingtalk requires client_id and client_secret")
	}
	if !c.DiscoveryOnly && c.AllowedSender == "" {
		return errors.New("dingtalk requires allowed_sender (this workspace is single-owner)")
	}
	return nil
}

// The owner must mention the bot in a group, including when all groups are enabled.
func (c DingTalkConfig) permits(sender, conversation, kind string, mentioned bool) bool {
	if !c.Enabled || c.DiscoveryOnly || sender == "" || sender != c.AllowedSender || conversation == "" {
		return false
	}
	listed := slices.Contains(c.AllowedConversations, conversation)
	switch kind {
	case "1":
		return len(c.AllowedConversations) == 0 || listed
	case "2":
		return mentioned && (c.AllowAllGroups || listed)
	default:
		return false
	}
}

type dingTalkMessage struct {
	MsgID            string `json:"msgId"`
	MsgType          string `json:"msgtype"`
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	Sender           string `json:"senderStaffId"`
	Mentioned        bool   `json:"isInAtList"`
	Webhook          string `json:"sessionWebhook"`
	ExpiresAt        int64  `json:"sessionWebhookExpiredTime"`
	OriginalMsgID    string `json:"originalMsgId"`
	Text             struct {
		IsReply bool `json:"isReplyMsg,omitempty"`
		Replied *struct {
			Type    string          `json:"msgType"`
			Content json.RawMessage `json:"content"`
		} `json:"repliedMsg,omitempty"`
		Content string `json:"content"`
	} `json:"text"`
}

// DingTalk supplies quoted text separately from the new user message. Only
// extract display text, never the raw message envelope or media credentials.
func (m dingTalkMessage) withQuote(body string, maxBytes int) string {
	q := m.Text.Replied
	if q == nil && !m.Text.IsReply && m.OriginalMsgID == "" {
		return body
	}
	var quoted string
	if q != nil && (q.Type == "text" || q.Type == "") {
		var object struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(q.Content, &object) == nil {
			quoted = object.Text
		} else {
			_ = json.Unmarshal(q.Content, &quoted)
		}
	}
	if opaqueDingTalkQuote.MatchString(quoted) {
		quoted = ""
	}
	return appendIMQuote(body, quoted, "钉钉", maxBytes)
}

var opaqueDingTalkQuote = regexp.MustCompile(`^[A-Za-z0-9+/=\s]+(?:\|\|[0-9]+)+$`)

type dingTalkFrame struct {
	Type    string            `json:"type"`
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
}
type dingTalkACK struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

func validDingTalkWebhook(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "oapi.dingtalk.com" && u.User == nil && u.Fragment == "" && u.Path == "/robot/sendBySession" && u.RawPath == "" && u.Query().Get("session") != ""
}

// ACK 200 means durable acceptance, NOT task completion. Policy refusals and
// unsupported messages are acknowledged without invoking the model. Only local
// persistence failures return 500 so DingTalk can retry the same msgId.
func (s *Service) acceptDingTalk(ctx context.Context, f dingTalkFrame) dingTalkACK {
	ack := dingTalkACK{Code: 200, Headers: map[string]string{"messageId": f.Headers["messageId"], "contentType": "application/json"}, Message: "OK", Data: "{}"}
	if f.Type == "SYSTEM" && f.Headers["topic"] == "ping" {
		ack.Data = f.Data
		return ack
	}
	if f.Type != "CALLBACK" || f.Headers["topic"] != dingTalkTopic {
		return ack
	}
	var m dingTalkMessage
	if json.Unmarshal([]byte(f.Data), &m) != nil || m.MsgID == "" {
		return ack
	}
	c := s.cfg.DingTalk
	if c.Enabled && c.DiscoveryOnly {
		s.log.Info("dingtalk discovery (no task accepted)", "sender_staff_id", m.Sender, "conversation_id", m.ConversationID, "conversation_type", m.ConversationType)
		return ack
	}
	if s.cfg.provider() != "dingtalk" || !c.permits(m.Sender, m.ConversationID, m.ConversationType, m.Mentioned) {
		if m.ConversationType == "2" && c.Enabled && m.Sender == c.AllowedSender {
			s.log.Info("dingtalk group message filtered", "conversation_id", m.ConversationID, "mentioned", m.Mentioned, "allowlisted", slices.Contains(c.AllowedConversations, m.ConversationID), "allow_all_groups", c.AllowAllGroups)
		}
		return ack
	}
	if m.MsgType != "text" {
		return ack
	}
	text := strings.TrimSpace(m.Text.Content)
	if text == "" || len(text) > s.cfg.Limits.MaxInputBytes || !validDingTalkWebhook(m.Webhook) || len(m.Webhook) > 8192 || m.ExpiresAt <= time.Now().UnixMilli() {
		return ack
	}
	p := Payload{Kind: "dingtalk", Text: m.withQuote(text, s.cfg.Limits.MaxInputBytes), DingTalk: &IMRoute{ConversationID: m.ConversationID, ConversationType: m.ConversationType, Sender: m.Sender, Mentioned: m.Mentioned, Webhook: m.Webhook, ExpiresAt: m.ExpiresAt}}
	key := "dingtalk:" + stableID(c.ClientID+"\x00"+m.ConversationID+"\x00"+m.MsgID)
	var eventID int64
	var inserted bool
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		var raw string
		e := tx.QueryRowContext(ctx, "SELECT id,payload FROM inbox WHERE dedup_key=?", key).Scan(&eventID, &raw)
		if e == nil {
			var old Payload
			if json.Unmarshal([]byte(raw), &old) != nil || old.DingTalk == nil {
				return ErrConflict
			}
			// Redelivery may refresh the expiring URL, but must not mutate the task or route.
			a, b := *old.DingTalk, *p.DingTalk
			a.Webhook = ""
			b.Webhook = ""
			a.ExpiresAt = 0
			b.ExpiresAt = 0
			if old.Text != p.Text || a != b {
				return ErrConflict
			}
			if p.DingTalk.ExpiresAt > old.DingTalk.ExpiresAt {
				old.DingTalk = p.DingTalk
				data, _ := json.Marshal(old)
				_, e = tx.ExecContext(ctx, "UPDATE inbox SET payload=? WHERE dedup_key=?", string(data), key)
			}
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		eventID, inserted, e = insertEvent(ctx, tx, key, p, time.Now().Add(time.Duration(s.cfg.Limits.DebounceMS)*time.Millisecond).UnixMilli())
		return e
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			s.log.Warn("dingtalk duplicate payload conflict")
			return ack
		}
		ack.Code = 500
		ack.Message = "inbox unavailable"
	} else {
		s.log.Info("dingtalk inbox accepted", "event_id", eventID, "inserted", inserted)
		s.notify()
	}
	return ack
}

func (s *Service) deliverDingTalk(ctx context.Context, n Notification) string {
	r, e := s.imRoute(ctx, s.read, n.Target)
	if e != nil || r.ExpiresAt <= time.Now().UnixMilli() || s.cfg.Permissions["send_message"] != "allow" {
		return "failed"
	}
	body, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": imText(n.Content)}})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, r.Webhook, bytes.NewReader(body))
	if e != nil {
		return "failed"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := s.client.Do(req)
	if e != nil {
		return "unknown"
	} // Never log a URL containing the session credential.
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return "pending"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if definiteRejection(resp.StatusCode) {
			return "failed"
		}
		return "unknown"
	}
	var result struct {
		Code *int `json:"errcode"`
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(raw) > 65536 || json.Unmarshal(raw, &result) != nil || result.Code == nil {
		return "unknown"
	}
	if *result.Code != 0 {
		s.log.Warn("dingtalk delivery rejected", "outbox_id", n.ID, "errcode", *result.Code)
		return "failed"
	}
	return "sent"
}

func (s *Service) dingTalkConnect(ctx context.Context) (*websocket.Conn, error) {
	body, _ := json.Marshal(map[string]any{"clientId": s.cfg.DingTalk.ClientID, "clientSecret": s.cfg.DingTalk.ClientSecret, "ua": "agentd/1.0", "subscriptions": []map[string]string{{"type": "CALLBACK", "topic": dingTalkTopic}}})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.dingtalk.com/v1.0/gateway/connections/open", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, e := s.client.Do(req)
	if e != nil {
		return nil, errors.New("dingtalk endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("dingtalk endpoint status %d", resp.StatusCode)
	}
	var endpoint struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&endpoint) != nil {
		return nil, errors.New("invalid dingtalk endpoint response")
	}
	u, e := url.Parse(endpoint.Endpoint)
	if e != nil || u.Scheme != "wss" || u.Hostname() != "wss-open-connection.dingtalk.com" || (u.Port() != "" && u.Port() != "443") || u.User != nil || u.Fragment != "" || endpoint.Ticket == "" {
		return nil, errors.New("untrusted dingtalk stream endpoint")
	}
	q := u.Query()
	q.Set("ticket", endpoint.Ticket)
	u.RawQuery = q.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, r, e := dialer.DialContext(ctx, u.String(), nil)
	if e != nil {
		if r != nil && r.Body != nil {
			r.Body.Close()
		}
		return nil, errors.New("dingtalk websocket handshake failed")
	}
	return conn, nil
}
func (s *Service) serveDingTalk(ctx context.Context, conn *websocket.Conn) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadLimit(int64(s.cfg.Limits.MaxInputBytes*2 + 32768))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	// Gorilla permits concurrent WriteControl with one ordinary writer (the ACK loop).
	pingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-tick.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f dingTalkFrame
		if e := conn.ReadJSON(&f); e != nil {
			return e
		}
		if f.Type == "SYSTEM" && f.Headers["topic"] == "disconnect" {
			return nil
		}
		ack := s.acceptDingTalk(ctx, f)
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if e := conn.WriteJSON(ack); e != nil {
			return e
		}
	}
}
func (s *Service) dingTalkLoop(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		conn, e := s.dingTalkConnect(ctx)
		if e == nil {
			s.mu.Lock()
			s.dingTalkConnected = true
			s.mu.Unlock()
			s.log.Info("dingtalk stream connected", "discovery_only", s.cfg.DingTalk.DiscoveryOnly)
			since := time.Now()
			_ = s.serveDingTalk(ctx, conn) // Don't log transport errors containing secret tickets.
			s.mu.Lock()
			s.dingTalkConnected = false
			s.mu.Unlock()
			if time.Since(since) > time.Minute {
				delay = time.Second
			}
		} else if ctx.Err() == nil {
			s.logErr("dingtalk connect", e)
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("dingtalk stream disconnected; reconnect scheduled", "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// Only the serial Outbox dispatcher accesses this short-lived token.
func (s *Service) deliverOwnerNews(ctx context.Context, n Notification) string {
	c := s.cfg.DingTalk
	if !s.canSendNews() || n.Target != s.newsTarget() {
		return "failed"
	}
	if s.robotToken == "" || !time.Now().Before(s.robotTokenUntil) {
		body, _ := json.Marshal(map[string]string{"appKey": c.ClientID, "appSecret": c.ClientSecret})
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.dingtalk.com/v1.0/oauth2/accessToken", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, e := s.client.Do(req)
		if e != nil {
			return "pending"
		}
		defer resp.Body.Close()
		var auth struct {
			AccessToken string
			ExpireIn    int64
		}
		if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&auth) != nil || auth.AccessToken == "" {
			s.log.Warn("dingtalk owner token unavailable", "http_status", resp.StatusCode)
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				return "pending"
			}
			return "failed"
		}
		s.robotToken = auth.AccessToken
		s.robotTokenUntil = time.Now().Add(time.Duration(auth.ExpireIn-60) * time.Second)
	}
	param, _ := json.Marshal(map[string]string{"content": imText(n.Content)})
	body, _ := json.Marshal(map[string]any{"robotCode": c.ClientID, "userIds": []string{c.AllowedSender}, "msgKey": "sampleText", "msgParam": string(param)})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.dingtalk.com/v1.0/robot/oToMessages/batchSend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", s.robotToken)
	resp, e := s.client.Do(req)
	if e != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	var result struct {
		ProcessQueryKey                               string
		Code                                          string
		InvalidStaffIdList, FlowControlledStaffIdList []string
	}
	e = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result)
	if resp.StatusCode == 401 {
		s.robotToken = ""
		return "pending"
	}
	if resp.StatusCode == 429 || len(result.FlowControlledStaffIdList) > 0 {
		return "pending"
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 || len(result.InvalidStaffIdList) > 0 {
		s.log.Warn("dingtalk owner push rejected", "http_status", resp.StatusCode, "code", result.Code)
		return "failed"
	}
	if resp.StatusCode != 200 || e != nil || result.ProcessQueryKey == "" {
		return "unknown"
	}
	return "sent"
}
