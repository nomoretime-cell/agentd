package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
