package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

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
type imSettings struct {
	Revision         string   `json:"revision"`
	Selected         string   `json:"selected"`
	Running          string   `json:"running"`
	Connected        bool     `json:"connected"`
	RunningDiscovery bool     `json:"running_discovery"`
	Restart          bool     `json:"restart"`
	Writable         bool     `json:"writable"`
	Channels         []imSlot `json:"channels"`
}
type imChange struct {
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

func (s *Service) imSettings(ctx context.Context) (imSettings, error) {
	s.imConfigMu.Lock()
	defer s.imConfigMu.Unlock()
	cfg, _, rev, e := s.imSnapshot()
	if e != nil {
		return imSettings{}, e
	}
	v := imSettings{Revision: rev, Selected: cfg.selectedIM(), Running: s.cfg.selectedIM(), Connected: s.im.connected(), Writable: s.writableIMConfig()}
	v.RunningDiscovery = v.Running == "wecom" && s.cfg.WeCom.DiscoveryOnly || v.Running == "dingtalk" && s.cfg.DingTalk.DiscoveryOnly
	v.Restart = v.Selected != v.Running || (v.Running == "wecom" && !reflect.DeepEqual(cfg.WeCom, s.cfg.WeCom)) || (v.Running == "dingtalk" && !reflect.DeepEqual(cfg.DingTalk, s.cfg.DingTalk))
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
		return mapping.Content[i+1].Encode(value)
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

func (s *Service) saveIM(ctx context.Context, req imChange) error {
	s.imConfigMu.Lock()
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
	p := req.Provider
	if p != "wecom" && p != "dingtalk" && !(p == "none" && req.Operation == "select") {
		return errors.New("未知的 IM 渠道")
	}
	var doc yaml.Node
	documents := yaml.NewDecoder(bytes.NewReader(raw))
	if documents.Decode(&doc) != nil || documents.Decode(new(yaml.Node)) != io.EOF || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return s.imFail("decode", errIMInvalid, errIMRead)
	}
	root := doc.Content[0]
	id, owner := cfg.imIdentity(p)
	originalID, originalOwner := id, owner
	prior, e := s.removedIdentity(p, cfg)
	if e != nil {
		return s.imFail("read identity", e, errIMRead)
	}
	if id != "" {
		prior = stableID(id + "\x00" + owner)
	}
	switch req.Operation {
	case "save":
		if v := strings.TrimSpace(req.ID); v != "" {
			id = v
		}
		if v := strings.TrimSpace(req.Owner); v != "" {
			owner = v
		}
		identity := stableID(id + "\x00" + owner)
		if prior != "" && prior != identity {
			// Discovery has not accepted tasks. Allow completing the owner identity
			// for this bot before turning discovery off.
			addingFirstOwner := originalID == id && originalOwner == "" && ((p == "wecom" && s.cfg.WeCom.DiscoveryOnly) || (p == "dingtalk" && s.cfg.DingTalk.DiscoveryOnly))
			if s.cfg.selectedIM() == p && !addingFirstOwner {
				return errors.New("请先停用或切换当前渠道并重启，再更换身份")
			}
			impact, err := s.imImpact(ctx, p)
			if err != nil {
				return err
			}
			if impact != (imImpact{}) {
				return errors.New("该渠道还有未结事件、动作或投递，请先用原身份完成核对")
			}
		}
		if strings.TrimSpace(req.Secret) == "" {
			req.Secret = ""
		}
		if p == "wecom" {
			c := &cfg.WeCom
			c.BotID = id
			c.Owner = owner
			if req.Secret != "" {
				c.Secret = req.Secret
			}
			c.DiscoveryOnly = req.Discovery
			c.AllowedGroups = req.Groups
			c.AllowAllGroups = req.AllowAllGroups
			if c.validate() != nil {
				return errIMIncomplete
			}
			e = setIMField(root, p, c)
		} else {
			c := &cfg.DingTalk
			c.ClientID = id
			c.AllowedSender = owner
			if req.Secret != "" {
				c.ClientSecret = req.Secret
			}
			c.DiscoveryOnly = req.Discovery
			c.AllowedConversations = req.Groups
			c.AllowAllGroups = req.AllowAllGroups
			check := *c
			check.Enabled = true
			if check.validate() != nil {
				return errIMIncomplete
			}
			e = setIMField(root, p, c)
		}
	case "select":
		if p == "dingtalk" {
			cfg.DingTalk.Enabled = true
			if cfg.DingTalk.validate() != nil {
				return errIMIncomplete
			}
			if e = setIMField(root, p, cfg.DingTalk); e != nil {
				return errIMWrite
			}
		}
		if p == "wecom" && cfg.WeCom.validate() != nil {
			return errIMIncomplete
		}
		cfg.IM.Provider = p
		e = setIMField(root, "im", cfg.IM)
	case "remove":
		if cfg.selectedIM() == p || s.cfg.selectedIM() == p {
			return errors.New("请先切换或停用该渠道，重启生效后再删除")
		}
		// A future add may restore parked work only to its original identity.
		// Record it under the state root before the config loses the identity.
		if prior != "" {
			if e = s.recordRemovedIdentity(p, prior); e != nil {
				return s.imFail("write identity", e, errIMWrite)
			}
		}
		e = setIMField(root, p, nil)
	default:
		return errors.New("未知的渠道操作")
	}
	if e != nil {
		return errIMWrite
	}
	next, e := yaml.Marshal(&doc)
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
	return nil
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
