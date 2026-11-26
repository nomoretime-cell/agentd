package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestIMSettingsLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.yaml")
	if err := os.WriteFile(path, []byte("# unrelated\ntimezone: Asia/Shanghai\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start := func() *Service {
		c, e := LoadConfig(path)
		if e != nil {
			t.Fatal(e)
		}
		s, e := Open(root, c, finalModel())
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	s := start()
	defer func() { s.Close() }()
	restart := func() {
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
		s = start()
	}
	view := func() imSettings {
		t.Helper()
		v, e := s.imSettings(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	change := func(c imChange) {
		t.Helper()
		c.Revision = view().Revision
		if e := s.saveIM(t.Context(), c); e != nil {
			t.Fatal(e)
		}
	}
	want := func(running, selected string, pending bool) {
		t.Helper()
		v := view()
		if v.Running != running || v.Selected != selected || v.Restart != pending || v.Connected {
			t.Fatalf("unexpected IM state: running=%s selected=%s restart=%t", v.Running, v.Selected, v.Restart)
		}
	}
	want("none", "none", false)
	add := imChange{Provider: "dingtalk", Operation: "save", ID: "example-client", Owner: "example-owner", Secret: "fixture-dummy-value"}
	stale := view().Revision
	change(add)
	if e := s.saveIM(t.Context(), imChange{Provider: "wecom", Operation: "select", Revision: stale}); !errors.Is(e, ErrConflict) {
		t.Fatal("stale update accepted")
	}
	change(imChange{Provider: "wecom", Operation: "save", ID: "example-wx-bot", Owner: "example-owner", Secret: "fixture-other-value"})
	change(imChange{Provider: "dingtalk", Operation: "save", Discovery: true})
	c, e := LoadConfig(path)
	if e != nil || c.DingTalk.ClientSecret != add.Secret || c.Timezone != "Asia/Shanghai" {
		t.Fatal("edit lost an unrelated value")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte("# unrelated")) {
		t.Fatal("unrelated comment lost")
	}
	b, _ := json.Marshal(view())
	for _, value := range []string{add.Secret, add.ID, add.Owner, "fixture-other-value"} {
		if strings.Contains(string(b), value) {
			t.Fatal("sensitive value in response")
		}
	}
	change(imChange{Provider: "wecom", Operation: "select"})
	want("none", "wecom", true)
	restart()
	want("wecom", "wecom", false)
	change(imChange{Provider: "dingtalk", Operation: "select"})
	want("wecom", "dingtalk", true)
	if e := s.saveIM(t.Context(), imChange{Provider: "wecom", Operation: "remove", Revision: view().Revision}); e == nil {
		t.Fatal("removed running configuration")
	}
	restart()
	want("dingtalk", "dingtalk", false)
	change(imChange{Provider: "wecom", Operation: "remove"})
	change(imChange{Provider: "none", Operation: "select"})
	restart()
	want("none", "none", false)
	for _, p := range []string{"dingtalk", "wecom"} {
		if _, e = s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES(?,?,0)", p, `{"kind":"`+p+`","text":"parked"}`); e != nil {
			t.Fatal(e)
		}
		if _, e = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES(?,?,?,0)", p, p+":1", "test"); e != nil {
			t.Fatal(e)
		}
	}
	// Owner-scoped news counts toward the provider it is parked for; a legacy
	// event without a scope belongs to DingTalk.
	for key, payload := range map[string]string{
		"news-dingtalk": `{"kind":"news_test","text":"parked"}`,
		"news-wecom":    `{"kind":"ai_news","text":"parked","news_target":"wecom-owner:x"}`,
	} {
		if _, e = s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES(?,?,0)", key, payload); e != nil {
			t.Fatal(e)
		}
	}
	batch, e := s.claim(t.Context())
	if e != nil || len(batch) != 0 {
		t.Fatal("disabled IM claimed an event", e)
	}
	if e = s.deliverOne(t.Context()); e != nil || count(t, s, "SELECT count(*) FROM outbox WHERE status='pending'") != 2 {
		t.Fatal("disabled IM consumed delivery", e)
	}
	change(imChange{Provider: "dingtalk", Operation: "remove"})
	if v := view(); v.Channels[1].Configured || v.Channels[1].Impact.Events != 2 || v.Channels[1].Impact.Deliveries != 1 {
		t.Fatal("removal lost parked work")
	}
	// The guard lives under the state root, not in the owner's config file.
	if raw, e := os.ReadFile(path); e != nil || strings.Contains(string(raw), "previous_identity") {
		t.Fatal("identity guard written to the config file", e)
	}
	if ids, e := s.removedIdentities(); e != nil || ids["dingtalk"] == "" {
		t.Fatal("identity guard not recorded", ids, e)
	}
	// Configs from earlier builds still guard a state root without an entry.
	legacy := Config{}
	legacy.IM.PreviousIdentity = map[string]string{"wecom": "legacy"}
	if v, e := openTest(t, testConfig(), nil).removedIdentity("wecom", legacy); e != nil || v != "legacy" {
		t.Fatal("legacy identity guard ignored", v, e)
	}
	add.ID = "different-app"
	add.Revision = view().Revision
	if e = s.saveIM(t.Context(), add); e == nil {
		t.Fatal("allowed rebind of parked work")
	}
	add.ID = "example-client"
	change(add)
	if stat, e := os.Stat(path); e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("private config permissions")
	}
}

func TestIMConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.yaml")
	if e := os.WriteFile(path, []byte("timezone: UTC\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	s := openTest(t, c, finalModel())
	v, e := s.imSettings(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 2)
	for _, p := range []string{"wecom", "dingtalk"} {
		go func() {
			result <- s.saveIM(t.Context(), imChange{Revision: v.Revision, Operation: "save", Provider: p, ID: "sample", Secret: "fixture", Discovery: true})
		}()
	}
	a, b := <-result, <-result
	if !(a == nil && errors.Is(b, ErrConflict) || b == nil && errors.Is(a, ErrConflict)) {
		t.Fatal("expected one committed edit and one conflict", a, b)
	}
}

// Saving a channel edits only its settings: comments inside the section and
// the file's two-space indentation stay as the owner wrote them.
func TestIMSaveKeepsFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.yaml")
	seed := "timezone: UTC # keep-top\nmodel:\n  name: x\nwecom:\n  # keep-head\n  team_knowledge: \"\" # keep-inner\n"
	if e := os.WriteFile(path, []byte(seed), 0644); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	s := openTest(t, c, finalModel())
	v, e := s.imSettings(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.saveIM(t.Context(), imChange{Provider: "wecom", Operation: "save", ID: "bot", Owner: "owner", Secret: "fixture-secret", Revision: v.Revision}); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"# keep-top", "# keep-head", "# keep-inner", "\n  name: x\n", "\n  bot_id: bot\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved config lacks %q:\n%s", want, raw)
		}
	}
	if stat, e := os.Stat(path); e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("config holding secrets not made private")
	}
}

// A configured but disabled DingTalk parks its news: claim and the
// workbench's unfinished-work count agree instead of running it unsent.
func TestDisabledDingTalkParksNews(t *testing.T) {
	c := testConfig()
	c.IM.Provider = "dingtalk"
	c.DingTalk.Enabled = false
	s := openTest(t, c, finalModel())
	if _, e := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES('legacy-news',?,0)", `{"kind":"ai_news"}`); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	batch, e := s.claim(t.Context())
	s.mu.Unlock()
	if e != nil || len(batch) != 0 {
		t.Fatal("disabled channel's news was claimed", batch, e)
	}
	impact, e := s.imImpact(t.Context(), "dingtalk")
	if e != nil || impact.Events != 1 {
		t.Fatal("parked news not counted", impact, e)
	}
}

// The real DingTalk adapter serves the socket and tracks connectivity. Only
// the platform's ticket exchange is replaced by a loopback handshake; the
// shared reconnect loop calls this connect too, so no retry leaves loopback.
type localDingTalk struct {
	*dingTalkChannel
	url string
}

func (c localDingTalk) connect(ctx context.Context) (imSession, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return nil, err
	}
	return dingTalkSession{c.dingTalkChannel, conn}, nil
}

func TestIMImmediateHandover(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.yaml")
	if err := os.WriteFile(path, []byte("timezone: UTC\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, cfg, finalModel())
	if err != nil {
		t.Fatal(err)
	}
	var reject atomic.Bool
	var hold atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	wx := fakeWeCom{alternateSecret: "rotated-secret", ready: make(chan *websocket.Conn, 8), posted: make(chan postedWeCom, 1)}
	wxServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if reject.Load() {
			http.Error(w, "test platform offline", 503)
			return
		}
		wx.serve(w, r)
	}))
	defer wxServer.Close()
	dingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		conn, e := up.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				return
			}
		}
	}))
	defer dingServer.Close()
	factory := func(s *Service, cfg *Config) imChannel {
		ch := newIMChannel(s, cfg)
		switch c := ch.(type) {
		case *weComChannel:
			c.url = strings.Replace(wxServer.URL, "http", "ws", 1)
		case *dingTalkChannel:
			return localDingTalk{c, strings.Replace(dingServer.URL, "http", "ws", 1)}
		}
		return ch
	}
	s.imLife.factory = factory
	s.Start(t.Context())
	defer func() { s.Close() }()
	view := func() imSettings {
		t.Helper()
		v, e := s.imSettings(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	save := func(req imChange) error { req.Revision = view().Revision; return s.saveIM(t.Context(), req) }
	success := func(req imChange) {
		t.Helper()
		if e := save(req); e != nil {
			t.Fatal(e)
		}
	}
	check := func(name string, connected bool) {
		t.Helper()
		waitFor(t, time.Second, func() bool {
			v := view()
			return v.Selected == name && v.Running == name && v.Connected == connected && !v.Restart
		})
		disk, e := LoadConfig(path)
		if e != nil || disk.selectedIM() != name {
			t.Fatalf("unexpected startup selection: %s, %v", disk.selectedIM(), e)
		}
	}
	ding := imChange{Operation: "save", Apply: true, Provider: "dingtalk", ID: "app", Owner: "alice", Secret: "test-ding"}
	success(ding)
	check("dingtalk", true)
	if s.ownerTarget() != "dingtalk-owner:alice" {
		t.Fatal("new owner is not used")
	}
	add := imChange{Operation: "save", Provider: "wecom", ID: "bot", Owner: "owner", Secret: "secret-wc"}
	success(add)
	check("dingtalk", true)
	rev := view().Revision
	req := imChange{Operation: "select", Apply: true, Revision: rev, Provider: "wecom"}
	reject.Store(true)
	hold.Store(true)
	done := make(chan error, 1)
	go func() { done <- s.saveIM(t.Context(), req) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not start")
	}
	pending := view()
	if pending.Attempt == nil || pending.Attempt.Error != "" || pending.Running != "dingtalk" || !pending.Connected {
		t.Error("missing handover status on refresh")
	}
	if e := s.saveIM(t.Context(), req); !errors.Is(e, ErrConflict) {
		t.Error("concurrent update accepted")
	}
	close(release)
	if e := <-done; e == nil {
		t.Fatal("invalid handshake accepted")
	}
	check("dingtalk", true)
	if v := view(); v.Attempt == nil || v.Attempt.Error == "" || v.Revision != rev {
		t.Fatal("missing retry or failed change committed")
	}
	reject.Store(false)
	success(imChange{Operation: "retry", Provider: "wecom", AttemptID: view().Attempt.ID})
	check("wecom", true)
	originalTarget := s.ownerTarget()
	if originalTarget != "wecom-owner:"+stableID("bot")+":owner" {
		t.Fatal("old owner still active")
	}
	success(imChange{Operation: "save", Apply: true, Provider: "wecom", Secret: "rotated-secret", Groups: []string{"new-room"}})
	if !s.imConfig().WeCom.allowsGroup("new-room") {
		t.Fatal("admission did not change")
	}
	// A rejected credential rotation must preserve the working credentials across restart.
	if e := save(imChange{Operation: "save", Apply: true, Provider: "wecom", Secret: "rejected-secret"}); e == nil {
		t.Fatal("invalid credential saved")
	}
	check("wecom", true)
	disk, e := LoadConfig(path)
	if e != nil || disk.WeCom.Secret != "rotated-secret" {
		t.Fatal("failed credential became startup credential")
	}
	b, _ := json.Marshal(view())
	if bytes.Contains(b, []byte("rejected-secret")) {
		t.Fatal("draft secret exposed")
	}
	// Restart from the actual saved file with the same local platform endpoints.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, disk, finalModel())
	if err != nil {
		t.Fatal(err)
	}
	s.imLife.factory = factory
	s.im.Store(&imRuntime{&s.cfg, factory(s, &s.cfg)})
	s.Start(t.Context())
	check("wecom", true)
	if view().Attempt != nil {
		t.Fatal("failed draft survived restart")
	}
	if _, e := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES('test',?,'test',9999999999999)", originalTarget); e != nil {
		t.Fatal(e)
	}
	success(imChange{Operation: "remove", Apply: true, Provider: "wecom"})
	check("none", false)
	if v := view(); v.Channels[0].Configured || v.Channels[0].Impact.Deliveries != 1 {
		t.Fatal("removal lost pending delivery")
	}
	add.ID = "another-bot"
	add.Apply = true
	if e := save(add); e == nil {
		t.Fatal("old delivery rebound to another bot")
	}
	success(imChange{Operation: "select", Apply: true, Provider: "dingtalk"})
	check("dingtalk", true)
	success(imChange{Operation: "select", Apply: true, Provider: "none"})
	check("none", false)
}
