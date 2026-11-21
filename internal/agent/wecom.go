package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type WeComConfig struct {
	BotID            string            `yaml:"bot_id"`
	Secret           string            `yaml:"secret"`
	Owner            string            `yaml:"owner_userid"`
	GroupOwnerTools  bool              `yaml:"group_owner_tools"`
	AllowAllGroups   bool              `yaml:"allow_all_groups"`
	AllowedGroups    []string          `yaml:"allowed_groups"`
	GroupLeads       map[string]string `yaml:"group_leads"`
	TeamKnowledge    string            `yaml:"team_knowledge"`
	ProgressMessages bool              `yaml:"progress_messages"`
	DiscoveryOnly    bool              `yaml:"discovery_only"`
}

var wecomUserID = regexp.MustCompile(`^[a-zA-Z0-9_.@=+/-]{1,128}$`)

func validMention(user string) bool {
	return wecomUserID.MatchString(user) && !strings.EqualFold(strings.TrimLeft(user, "@"), "all")
}
func (c WeComConfig) validate() error {
	if c.BotID == "" || c.Secret == "" || (!c.DiscoveryOnly && c.Owner == "") {
		return errors.New("wecom requires bot_id, secret and owner_userid (owner optional in discovery mode)")
	}
	for group, user := range c.GroupLeads {
		if !c.allowsGroup(group) || !validMention(user) {
			return errors.New("wecom group_leads requires an enabled group and a specific userid")
		}
	}
	return nil
}
func (c WeComConfig) allowsGroup(group string) bool {
	return group != "" && (c.AllowAllGroups || slices.Contains(c.AllowedGroups, group))
}

func (c WeComConfig) permits(r *IMRoute) bool {
	if c.DiscoveryOnly || r.BotID == "" || r.BotID != c.BotID || r.Sender == "" || r.ConversationID == "" {
		return false
	}
	switch r.ConversationType {
	case "single":
		return r.Sender == c.Owner && r.ConversationID == r.Sender
	case "group":
		return c.allowsGroup(r.ConversationID)
	}
	return false
}

type wecomFrame struct {
	Cmd     string `json:"cmd,omitempty"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body json.RawMessage `json:"body,omitempty"`
	Code *int            `json:"errcode,omitempty"`
}

func (s *Service) acceptWeCom(ctx context.Context, raw json.RawMessage) error {
	var m struct {
		MsgID  string `json:"msgid"`
		BotID  string `json:"aibotid"`
		ChatID string `json:"chatid"`
		Kind   string `json:"chattype"`
		Type   string `json:"msgtype"`
		From   struct {
			UserID string `json:"userid"`
		} `json:"from"`
		Text struct {
			Content string `json:"content"`
		} `json:"text"`
		Quote *struct {
			Type string `json:"msgtype"`
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"quote"`
	}
	if json.Unmarshal(raw, &m) != nil || m.MsgID == "" || m.BotID != s.cfg.WeCom.BotID {
		return nil
	}
	if m.Kind == "single" {
		m.ChatID = m.From.UserID
	}
	r := &IMRoute{BotID: m.BotID, ConversationID: m.ChatID, ConversationType: m.Kind, Sender: m.From.UserID}
	if s.cfg.WeCom.DiscoveryOnly {
		s.log.Info("wecom discovery", "userid", r.Sender, "chatid", r.ConversationID, "type", r.ConversationType)
		return nil
	}
	text := strings.TrimSpace(m.Text.Content)
	if s.cfg.provider() != "wecom" {
		return nil
	}
	if !s.cfg.WeCom.permits(r) {
		if r.team() && r.Sender == s.cfg.WeCom.Owner && r.ConversationID != "" {
			s.log.Info("wecom owner mention in unlisted group", "chatid", r.ConversationID)
		}
		return nil
	}
	if m.Type != "text" || text == "" || len(text) > s.cfg.Limits.MaxInputBytes {
		return nil
	}
	// Only the authenticated owner can select a different lead for this task.
	if r.team() && r.Sender == s.cfg.WeCom.Owner && strings.HasPrefix(text, "/提醒 ") {
		fields := strings.Fields(text)
		if len(fields) >= 3 && validMention(fields[1]) {
			r.NotifyUser = fields[1]
			text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(text, "/提醒")), fields[1]))
		}
	}
	if m.Quote != nil {
		quoted := ""
		if m.Quote.Type == "text" {
			quoted = m.Quote.Text.Content
		}
		text = appendIMQuote(text, quoted, "企微", s.cfg.Limits.MaxInputBytes)
	}
	key := "wecom:" + stableID(strings.Join([]string{m.BotID, m.Kind, m.ChatID, m.MsgID}, "\x00"))
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		_, _, err := insertEvent(ctx, tx, key, Payload{Kind: "wecom", WeCom: r, Text: text}, time.Now().Add(time.Duration(s.cfg.Limits.DebounceMS)*time.Millisecond).UnixMilli())
		return err
	})
	if errors.Is(err, ErrConflict) {
		s.log.Warn("wecom duplicate payload conflict")
		return nil
	}
	if err == nil {
		s.notify()
	}
	return err
}

type wecomSend struct {
	body any
	ack  chan string
}
type wecomConnection struct {
	send chan wecomSend
	done chan struct{}
}

func (s *Service) deliverWeCom(ctx context.Context, n Notification) string {
	if s.cfg.Permissions["send_message"] != "allow" {
		return "failed"
	}
	chat := s.cfg.WeCom.Owner
	content := strings.ReplaceAll(n.Content, "<@", "＜@")
	if n.Target == s.newsTarget() {
		if !s.canSendNews() {
			return "failed"
		}
	} else {
		r, err := s.imRoute(ctx, s.read, n.Target)
		if err != nil || r.BotID == "" {
			return "failed"
		}
		chat = r.ConversationID
		lead := s.cfg.WeCom.GroupLeads[chat]
		if r.Sender == s.cfg.WeCom.Owner && validMention(r.NotifyUser) {
			lead = r.NotifyUser
		}
		if r.team() && lead != "" && (strings.HasPrefix(n.Key, "reply:") || strings.HasPrefix(n.Key, "message:")) {
			content = "<@" + lead + ">\n" + content
		}
	}
	body := map[string]any{"chatid": chat, "msgtype": "markdown", "markdown": map[string]string{"content": imText(content)}}
	s.mu.Lock()
	session := s.wecomConn
	s.mu.Unlock()
	if session == nil {
		return "offline"
	}
	req := wecomSend{body: body, ack: make(chan string, 1)}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case session.send <- req:
	case <-ctx.Done():
		return "offline"
	case <-session.done:
		return "offline"
	}
	select {
	case status := <-req.ack:
		return status
	case <-ctx.Done():
		return "unknown"
	case <-session.done:
		select {
		case status := <-req.ack:
			return status
		default:
			return "unknown"
		}
	}
}

func (s *Service) wecomLoop(ctx context.Context) {

	backoff := time.Second
	for ctx.Err() == nil {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, s.wecomURL, nil)
		if err == nil {
			start := time.Now()
			err = s.serveWeCom(ctx, conn)
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("wecom disconnected", "retry_in", backoff, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(30*time.Second, backoff*2)
	}
}

func (s *Service) serveWeCom(ctx context.Context, conn *websocket.Conn) error {
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	write := func(cmd, id string, body any) error {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		frame := map[string]any{"cmd": cmd, "headers": map[string]string{"req_id": id}}
		if body != nil {
			frame["body"] = body
		}
		return conn.WriteJSON(frame)
	}
	sub := randomID()
	if err := write("aibot_subscribe", sub, map[string]string{"bot_id": s.cfg.WeCom.BotID, "secret": s.cfg.WeCom.Secret}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var auth wecomFrame
	if err := conn.ReadJSON(&auth); err != nil {
		return err
	}
	if auth.Headers.ReqID != sub || auth.Code == nil || *auth.Code != 0 {
		return errors.New("wecom subscription rejected")
	}
	conn.SetReadDeadline(time.Time{})
	session := &wecomConnection{send: make(chan wecomSend), done: make(chan struct{})}
	s.mu.Lock()
	s.wecomConn = session
	s.mu.Unlock()
	s.log.Info("wecom connected", "discovery_only", s.cfg.WeCom.DiscoveryOnly)
	frames := make(chan wecomFrame)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(frames)
		for {
			var f wecomFrame
			if conn.ReadJSON(&f) != nil {
				return
			}
			select {
			case frames <- f:
			case <-session.done:
				return
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		s.wecomConn = nil
		s.mu.Unlock()
		close(session.done)
		conn.Close()
		<-readerDone
	}()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var pending, ping string
	var ack chan string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if ping != "" {
				return errors.New("missing wecom heartbeat")
			}
			ping = randomID()
			if err := write("ping", ping, nil); err != nil {
				return err
			}
		case req := <-session.send:
			pending = randomID()
			ack = req.ack
			if err := write("aibot_send_msg", pending, req.body); err != nil {
				return err
			}
		case f, ok := <-frames:
			if !ok {
				return errors.New("wecom stream closed")
			}
			switch {
			case f.Cmd == "aibot_msg_callback":
				if err := s.acceptWeCom(ctx, f.Body); err != nil {
					return err
				}
			case f.Cmd == "aibot_event_callback":
				var event struct {
					Event struct {
						Type string `json:"eventtype"`
					} `json:"event"`
				}
				if json.Unmarshal(f.Body, &event) == nil && event.Event.Type == "disconnected_event" {
					return errors.New("wecom disconnected event")
				}
			case f.Cmd == "" && ping != "" && f.Headers.ReqID == ping:
				if f.Code == nil || *f.Code != 0 {
					return errors.New("wecom heartbeat rejected")
				}
				ping = ""
			case f.Cmd == "" && pending != "" && f.Headers.ReqID == pending:
				status := "unknown"
				if f.Code != nil {
					switch *f.Code {
					case 0:
						status = "sent"
					case 45009:
						status = "pending"
					default:
						status = "failed"
					}
					if *f.Code != 0 {
						s.log.Warn("wecom send rejected", "errcode", *f.Code)
					}
				}
				ack <- status
				pending = ""
			}
		}
	}
}
