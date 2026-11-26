package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type imImpact struct {
	Events     int `json:"events"`
	Actions    int `json:"actions"`
	Deliveries int `json:"deliveries"`
}
type imSlot struct {
	Provider       string   `json:"provider"`
	ID             string   `json:"id"`
	Owner          string   `json:"owner"`
	HasSecret      bool     `json:"has_secret"`
	Configured     bool     `json:"configured"`
	Discovery      bool     `json:"discovery"`
	AllowAllGroups bool     `json:"allow_all_groups"`
	Groups         []string `json:"groups"`
	Impact         imImpact `json:"impact"`
}

// Only the projected status is sent back to the browser. A failed draft may
// contain a new secret; it stays in process memory for an explicit retry.
type imAttempt struct {
	ID       string `json:"id"`
	request  imChange
	Provider string `json:"provider"`
	Error    string `json:"error,omitempty"`
}
type imSettings struct {
	Attempt          *imAttempt `json:"attempt,omitempty"`
	Revision         string     `json:"revision"`
	Selected         string     `json:"selected"`
	Running          string     `json:"running"`
	Connected        bool       `json:"connected"`
	RunningDiscovery bool       `json:"running_discovery"`
	Restart          bool       `json:"restart"`
	Writable         bool       `json:"writable"`
	Channels         []imSlot   `json:"channels"`
}
type imChange struct {
	AttemptID      string   `json:"attempt_id,omitempty"`
	Apply          bool     `json:"apply,omitempty"`
	Revision       string   `json:"revision"`
	Operation      string   `json:"operation"`
	Provider       string   `json:"provider"`
	ID             string   `json:"id"`
	Owner          string   `json:"owner"`
	Secret         string   `json:"secret"`
	Discovery      bool     `json:"discovery"`
	AllowAllGroups bool     `json:"allow_all_groups"`
	Groups         []string `json:"groups"`
}

func (c Config) selectedIM() string {
	if c.provider() == "dingtalk" && !c.DingTalk.Enabled {
		return "none"
	}
	return c.provider()
}

func maskedIMID(value string) string {
	r := []rune(value)
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 5 {
		return "••••"
	}
	return "••••" + string(r[len(r)-4:])
}

func (c Config) imIdentity(p string) (string, string) {
	if p == "wecom" {
		return c.WeCom.BotID, c.WeCom.Owner
	}
	return c.DingTalk.ClientID, c.DingTalk.AllowedSender
}

func (s *Service) imImpact(ctx context.Context, p string) (imImpact, error) {
	// Include approvals already decided but whose continuation is still queued.
	// Owner-scoped kinds and their scope field come from the business that
	// parks them (scopedKinds, scopeField), so this count matches claim.
	// Legacy DingTalk events carry an empty scope, as eventScope documents.
	var impact imImpact
	err := s.read.QueryRowContext(ctx, `WITH origins AS (
 SELECT id FROM inbox WHERE json_extract(payload,'$.kind')=?1 OR (
 json_extract(payload,'$.kind') IN (SELECT value FROM json_each(?4)) AND
 (json_extract(payload,?5) GLOB ?2 OR (?1='dingtalk' AND coalesce(json_extract(payload,?5),'')='')))),
 related AS (SELECT a.id, a.status FROM actions a JOIN origins ON origins.id=CAST(substr(a.dedup_key,4) AS INTEGER) WHERE a.dedup_key GLOB 'ev:[0-9]*:*')
 SELECT (SELECT count(*) FROM inbox WHERE status!='done' AND (id IN origins OR (json_extract(payload,'$.kind')='approval' AND json_extract(payload,'$.action_id') IN (SELECT id FROM related)))),
 (SELECT count(*) FROM related WHERE status IN ('pending_approval','approved','executing','unknown')),
 (SELECT count(*) FROM outbox WHERE status!='sent' AND (target GLOB ?2 OR target GLOB ?3))`, p, p+"-owner:*", p+":*", scopedKinds, scopeField).Scan(&impact.Events, &impact.Actions, &impact.Deliveries)
	return impact, err
}

var errIMRead = errors.New("无法读取有效的启动配置，请在服务器检查配置文件")
var errIMWrite = errors.New("无法保存私有配置，请检查文件权限")

// errIMInvalid is logged in place of YAML decode errors, which can quote
// configuration values, including secrets that redaction does not know yet.
var errIMInvalid = errors.New("config file does not decode or validate; start agentd with it to see the cause")

// imFail records the cause for the operator once and returns the message the
// workbench may show. File-system errors carry the path, never file content.
func (s *Service) imFail(op string, cause, public error) error {
	s.logErr("im config "+op, cause)
	return public
}

var errIMIncomplete = errors.New("请填写应用 ID 和密钥，关闭仅发现模式时还须填写所有者 ID；请核对群准入设置")

// imIdentityFile keeps the identity fingerprint of each removed channel under
// the state root, so it is backed up with the work it protects and survives
// edits to the config file.
const imIdentityFile = "data/im-identity.json"

// removedIdentity returns the fingerprint a re-added channel must match.
// Builds before imIdentityFile stored it as im.previous_identity in the
// config file, which is still honoured when the state root has no entry.
func (s *Service) removedIdentity(p string, cfg Config) (string, error) {
	ids, e := s.removedIdentities()
	if v, ok := ids[p]; ok || e != nil {
		return v, e
	}
	return cfg.IM.PreviousIdentity[p], nil
}
func (s *Service) removedIdentities() (map[string]string, error) {
	ids := map[string]string{}
	b, e := os.ReadFile(filepath.Join(s.root, imIdentityFile))
	if errors.Is(e, fs.ErrNotExist) {
		return ids, nil
	}
	if e == nil {
		e = json.Unmarshal(b, &ids)
	}
	return ids, e
}

// recordRemovedIdentity runs with imConfigMu held.
func (s *Service) recordRemovedIdentity(p, fingerprint string) error {
	ids, e := s.removedIdentities()
	if e != nil {
		return e
	}
	ids[p] = fingerprint
	raw, e := json.Marshal(ids)
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.root, imIdentityFile), raw, 0600)
}

func (s *Service) imSnapshot() (Config, []byte, string, error) {
	if s.cfg.sourcePath == "" {
		return s.cfg, nil, "", nil
	}
	data, e := os.ReadFile(s.cfg.sourcePath)
	if e != nil {
		return Config{}, nil, "", s.imFail("read", e, errIMRead)
	}
	cfg, e := decodeConfig(bytes.NewReader(data))
	if e != nil {
		return Config{}, nil, "", s.imFail("decode", errIMInvalid, errIMRead)
	}
	mac := hmac.New(sha256.New, []byte(s.token))
	mac.Write(data)
	return cfg, data, hex.EncodeToString(mac.Sum(nil)), nil
}
func (s *Service) writableIMConfig() bool {
	path, err := filepath.EvalSymlinks(s.cfg.sourcePath)
	// The distributed config.yaml is a checked-in example. The normal private
	// configuration is config.local.yaml; standalone installations may use -config.
	return err == nil && path != "" && !(filepath.Base(path) == "config.yaml" && fileExists(filepath.Join(filepath.Dir(path), ".git")))
}

// Only the selected platform affects the running channel. Other slots can
// be saved independently without interrupting its connection.
func (cfg *Config) sameActiveIM(other *Config) bool {
	if cfg.selectedIM() != other.selectedIM() {
		return false
	}
	switch cfg.selectedIM() {
	case "wecom":
		return reflect.DeepEqual(cfg.WeCom, other.WeCom)
	case "dingtalk":
		return reflect.DeepEqual(cfg.DingTalk, other.DingTalk)
	default:
		return true
	}
}

func (s *Service) imSettings(ctx context.Context) (imSettings, error) {
	cfg, _, rev, e := s.imSnapshot()
	if e != nil {
		return imSettings{}, e
	}
	live := s.im.Load()
	v := imSettings{Revision: rev, Selected: cfg.selectedIM(), Running: live.cfg.selectedIM(), Connected: live.channel.connected(), Writable: s.writableIMConfig()}
	v.RunningDiscovery = v.Running == "wecom" && live.cfg.WeCom.DiscoveryOnly || v.Running == "dingtalk" && live.cfg.DingTalk.DiscoveryOnly
	v.Restart = !live.cfg.sameActiveIM(&cfg)
	if attempt := s.imLife.attempt.Load(); attempt != nil && (attempt.Error == "" || attempt.request.Revision == rev) {
		v.Attempt = attempt
	}
	for _, p := range []string{"wecom", "dingtalk"} {
		id, owner := cfg.imIdentity(p)
		slot := imSlot{Provider: p, ID: maskedIMID(id), Owner: maskedIMID(owner)}
		if p == "wecom" {
			c := cfg.WeCom
			slot.HasSecret = c.Secret != ""
			slot.Discovery = c.DiscoveryOnly
			slot.Groups = c.AllowedGroups
			slot.AllowAllGroups = c.AllowAllGroups
		} else {
			c := cfg.DingTalk
			slot.HasSecret = c.ClientSecret != ""
			slot.Discovery = c.DiscoveryOnly
			slot.Groups = c.AllowedConversations
			slot.AllowAllGroups = c.AllowAllGroups
		}
		slot.Configured = id != "" || owner != "" || slot.HasSecret
		if slot.Groups == nil {
			slot.Groups = []string{}
		}
		slot.Impact, e = s.imImpact(ctx, p)
		if e != nil {
			return v, e
		}
		v.Channels = append(v.Channels, slot)
	}
	return v, nil
}

// Replace only edited settings, keeping unrelated YAML nodes, including comments.
func setIMField(mapping *yaml.Node, key string, value any) error {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if value == nil {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return nil
		}
		var fresh yaml.Node
		if e := fresh.Encode(value); e != nil {
			return e
		}
		mergeYAML(mapping.Content[i+1], &fresh)
		return nil
	}
	if value == nil {
		return nil
	}
	var node yaml.Node
	if e := node.Encode(value); e != nil {
		return e
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
	return nil
}

// mergeYAML writes fresh into old key by key, so the owner's comments and
// keys the settings do not cover stay in place. Replaced values keep the
// comments of the values they replace.
func mergeYAML(old, fresh *yaml.Node) {
	if old.Kind != yaml.MappingNode || fresh.Kind != yaml.MappingNode {
		head, line, foot := old.HeadComment, old.LineComment, old.FootComment
		*old = *fresh
		old.HeadComment, old.LineComment, old.FootComment = head, line, foot
		return
	}
	for j := 0; j < len(fresh.Content); j += 2 {
		found := false
		for i := 0; i < len(old.Content); i += 2 {
			if old.Content[i].Value == fresh.Content[j].Value {
				mergeYAML(old.Content[i+1], fresh.Content[j+1])
				found = true
				break
			}
		}
		if !found {
			old.Content = append(old.Content, fresh.Content[j], fresh.Content[j+1])
		}
	}
}

// encodeConfig keeps the two-space indentation of the shipped configs.
func encodeConfig(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if e := enc.Encode(doc); e != nil {
		return nil, e
	}
	if e := enc.Close(); e != nil {
		return nil, e
	}
	return buf.Bytes(), nil
}

func (s *Service) saveIM(ctx context.Context, req imChange) error {
	if !s.imConfigMu.TryLock() {
		return ErrConflict
	}
	defer s.imConfigMu.Unlock()
	if !s.writableIMConfig() {
		return errors.New("请使用私有配置文件（例如 config.local.yaml）启动服务后编辑")
	}
	cfg, raw, rev, e := s.imSnapshot()
	if e != nil {
		return e
	}
	if req.Revision != rev {
		return ErrConflict
	}
	if req.Operation == "retry" {
		if req, e = s.retriedIM(req, rev); e != nil {
			return e
		}
	}
	p := req.Provider
	if p != "wecom" && p != "dingtalk" && !(p == "none" && req.Operation == "select") {
		return errors.New("未知的 IM 渠道")
	}
	var doc yaml.Node
	documents := yaml.NewDecoder(bytes.NewReader(raw))
	if documents.Decode(&doc) != nil || documents.Decode(new(yaml.Node)) != io.EOF || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return s.imFail("decode", errIMInvalid, errIMRead)
	}
	if e = s.editIM(ctx, doc.Content[0], &cfg, req); e != nil {
		return e
	}
	next, e := encodeConfig(&doc)
	if e != nil {
		return errIMWrite
	}
	persisted, e := decodeConfig(bytes.NewReader(next))
	if e != nil {
		return errIMRead
	}
	if req.Operation == "remove" {
		defaults := DefaultConfig()
		if (p == "wecom" && !reflect.DeepEqual(persisted.WeCom, defaults.WeCom)) || (p == "dingtalk" && !reflect.DeepEqual(persisted.DingTalk, defaults.DingTalk)) {
			return errors.New("不能删除继承自其他 YAML 段的渠道配置，请在服务器处理")
		}
	}
	write := func() error {
		path, e := filepath.EvalSymlinks(s.cfg.sourcePath)
		if e != nil {
			return s.imFail("resolve", e, errIMRead)
		}
		current, e := os.ReadFile(path)
		if e != nil {
			return s.imFail("read", e, errIMRead)
		}
		if !bytes.Equal(current, raw) {
			return ErrConflict
		}
		if e = atomicWrite(path, next, 0600); e != nil {
			return s.imFail("write", e, errIMWrite)
		}
		s.rememberIMSecrets(persisted)
		return nil
	}
	if req.Apply {
		e = s.applyIM(req, &persisted, write)
	} else {
		e = write()
	}
	if e == nil {
		s.imLife.attempt.Store(nil)
	}
	return e
}

// retriedIM returns the failed request that an explicit retry repeats.
func (s *Service) retriedIM(req imChange, rev string) (imChange, error) {
	prior := s.imLife.attempt.Load()
	if prior == nil || prior.Error == "" || prior.ID != req.AttemptID || prior.Provider != req.Provider || prior.request.Revision != rev {
		return imChange{}, ErrConflict
	}
	return prior.request, nil
}

// editIM applies one owner operation to the config and its YAML document.
// prior is the identity that unfinished work of the channel belongs to.
func (s *Service) editIM(ctx context.Context, root *yaml.Node, cfg *Config, req imChange) error {
	p := req.Provider
	prior, e := s.removedIdentity(p, *cfg)
	if e != nil {
		return s.imFail("read identity", e, errIMRead)
	}
	if id, owner := cfg.imIdentity(p); id != "" {
		prior = stableID(id + "\x00" + owner)
	}
	switch req.Operation {
	case "save":
		if e = s.saveIMSlot(ctx, root, cfg, req, prior); e != nil || !req.Apply {
			return e
		}
		return selectIM(root, cfg, p)
	case "select":
		return selectIM(root, cfg, p)
	case "remove":
		return s.removeIMSlot(root, cfg, p, req.Apply, prior)
	}
	return errors.New("未知的渠道操作")
}

// saveIMSlot updates one platform's settings. Its identity can change only
// when no unfinished work still belongs to the previous one.
func (s *Service) saveIMSlot(ctx context.Context, root *yaml.Node, cfg *Config, req imChange, prior string) error {
	p := req.Provider
	originalID, originalOwner := cfg.imIdentity(p)
	id, owner := originalID, originalOwner
	if v := strings.TrimSpace(req.ID); v != "" {
		id = v
	}
	if v := strings.TrimSpace(req.Owner); v != "" {
		owner = v
	}
	if prior != "" && prior != stableID(id+"\x00"+owner) {
		running := s.imConfig()
		// Discovery has not accepted tasks. Allow completing the owner identity
		// for this bot before turning discovery off.
		addingFirstOwner := originalID == id && originalOwner == "" && ((p == "wecom" && running.WeCom.DiscoveryOnly) || (p == "dingtalk" && running.DingTalk.DiscoveryOnly))
		if running.selectedIM() == p && !addingFirstOwner {
			return errors.New("请先停用或切换当前渠道，再更换机器人或所有者身份")
		}
		impact, err := s.imImpact(ctx, p)
		if err != nil {
			return err
		}
		if impact != (imImpact{}) {
			return errors.New("该渠道还有未结事件、动作或投递，请先用原身份完成核对")
		}
	}
	secret := req.Secret
	if strings.TrimSpace(secret) == "" {
		secret = ""
	}
	var section any
	if p == "wecom" {
		c := &cfg.WeCom
		c.BotID, c.Owner = id, owner
		if secret != "" {
			c.Secret = secret
		}
		c.DiscoveryOnly, c.AllowedGroups, c.AllowAllGroups = req.Discovery, req.Groups, req.AllowAllGroups
		if c.validate() != nil {
			return errIMIncomplete
		}
		section = c
	} else {
		c := &cfg.DingTalk
		c.ClientID, c.AllowedSender = id, owner
		if secret != "" {
			c.ClientSecret = secret
		}
		c.DiscoveryOnly, c.AllowedConversations, c.AllowAllGroups = req.Discovery, req.Groups, req.AllowAllGroups
		check := *c
		check.Enabled = true
		if check.validate() != nil {
			return errIMIncomplete
		}
		section = c
	}
	if setIMField(root, p, section) != nil {
		return errIMWrite
	}
	return nil
}

// selectIM makes p the channel the next start uses.
func selectIM(root *yaml.Node, cfg *Config, p string) error {
	if p == "dingtalk" {
		cfg.DingTalk.Enabled = true
		if cfg.DingTalk.validate() != nil {
			return errIMIncomplete
		}
		if setIMField(root, p, cfg.DingTalk) != nil {
			return errIMWrite
		}
	}
	if p == "wecom" && cfg.WeCom.validate() != nil {
		return errIMIncomplete
	}
	cfg.IM.Provider = p
	if setIMField(root, "im", cfg.IM) != nil {
		return errIMWrite
	}
	return nil
}

// removeIMSlot deletes one platform's settings. A selected channel is
// deselected only together with an immediate apply. The removed identity is
// recorded first, so only it can later resume the channel's parked work.
func (s *Service) removeIMSlot(root *yaml.Node, cfg *Config, p string, apply bool, prior string) error {
	running := s.imConfig().selectedIM()
	if cfg.selectedIM() == p || running == p {
		if !apply {
			return errors.New("请先切换或停用该渠道，再删除配置")
		}
		// Keep the actual other channel when deleting a legacy next-start selection.
		cfg.IM.Provider = running
		if running == p {
			cfg.IM.Provider = "none"
		}
		if setIMField(root, "im", cfg.IM) != nil {
			return errIMWrite
		}
	}
	if prior != "" {
		if e := s.recordRemovedIdentity(p, prior); e != nil {
			return s.imFail("write identity", e, errIMWrite)
		}
	}
	if setIMField(root, p, nil) != nil {
		return errIMWrite
	}
	return nil
}

// applyIM authenticates the new channel before persist commits the next
// startup selection, so a failure or a crash keeps the previous one. The
// latest attempt stays readable for a page refresh and an explicit retry.
func (s *Service) applyIM(req imChange, cfg *Config, persist func() error) error {
	if s.imLife.ctx == nil || s.imLife.ctx.Err() != nil {
		return errors.New("渠道服务尚未运行")
	}
	live := s.im.Load()
	if live.cfg.sameActiveIM(cfg) && (cfg.selectedIM() == "none" || live.channel.connected()) {
		return persist()
	}
	attempt := imAttempt{ID: randomID(), request: req, Provider: req.Provider}
	if attempt.Provider == "none" {
		attempt.Provider = live.cfg.selectedIM()
	}
	s.imLife.attempt.Store(&attempt)
	err := s.handoverIM(live, cfg, persist)
	if err != nil {
		failed := attempt
		failed.Error = err.Error()
		s.imLife.attempt.Store(&failed)
	}
	return err
}

// handoverIM replaces the running channel with one built from cfg. A second
// subscription for the same bot can evict the first, so the running channel
// is stopped before probing it again; another platform is probed while the
// current one keeps working. On failure whatever was stopped runs again.
func (s *Service) handoverIM(live *imRuntime, cfg *Config, persist func() error) error {
	candidate := s.imLife.factory(s, cfg)
	held := false
	if live.cfg.selectedIM() == cfg.selectedIM() {
		if e := s.holdIM(); e != nil {
			return e
		}
		held = true
	}
	session, err := s.probeIM(candidate)
	if err == nil && !held {
		err = s.holdIM()
		held = err == nil
	}
	if err == nil && s.imLife.ctx.Err() != nil {
		err = errors.New("服务正在退出，配置未应用")
	}
	if err == nil {
		err = persist()
	}
	if err == nil {
		s.im.Store(&imRuntime{cfg, candidate})
		s.startIM(session)
	} else {
		if session != nil {
			session.Close()
		}
		if held && s.imLife.ctx.Err() == nil {
			s.startIM(nil)
		}
	}
	if held {
		s.releaseIM()
	}
	return err
}

// holdIM stops the running channel once no task or IM delivery is in flight.
// Inbox, Timers and Outbox start no new work until releaseIM.
func (s *Service) holdIM() error {
	s.mu.Lock()
	if s.activeCancel != nil || s.imLife.sending {
		s.mu.Unlock()
		return errors.New("当前任务或投递正在执行，请稍后重试")
	}
	s.imLife.holding = true
	s.mu.Unlock()
	s.stopIM()
	return nil
}

func (s *Service) releaseIM() {
	s.mu.Lock()
	s.imLife.holding = false
	s.mu.Unlock()
	s.notify()
}

// probeIM authenticates a channel within a bounded time.
func (s *Service) probeIM(channel imChannel) (imSession, error) {
	ctx, cancel := context.WithTimeout(s.imLife.ctx, 12*time.Second)
	defer cancel()
	session, e := channel.connect(ctx)
	if e == nil {
		return session, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("连接平台超时，请检查网络后重试")
	}
	return nil, fmt.Errorf("连接失败：%w", e)
}

func (s *Service) registerIMManagement(m *http.ServeMux) {
	m.HandleFunc("GET /v1/im", func(w http.ResponseWriter, r *http.Request) { v, e := s.imSettings(r.Context()); respond(w, v, e) })
	m.HandleFunc("POST /v1/im", func(w http.ResponseWriter, r *http.Request) {
		var req imChange
		// Never echo a malformed credential-bearing request, even an unknown key.
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
			respond(w, nil, errors.New("无效的渠道配置请求"))
			return
		}
		e := s.saveIM(r.Context(), req)
		respond(w, map[string]bool{"saved": e == nil}, e)
	})
}
