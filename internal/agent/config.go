package agent

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type NewsFeed struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Language string `yaml:"language"`
}

type Config struct {
	IM struct {
		Provider string `yaml:"provider"`
	} `yaml:"im"`
	WeCom              WeComConfig          `yaml:"wecom"`
	NewsFeeds          []NewsFeed           `yaml:"news_feeds"`
	OpenCodeBinary     string               `yaml:"opencode_binary"`
	DingTalk           DingTalkConfig       `yaml:"dingtalk"`
	Listen             string               `yaml:"listen"`
	Model              ModelConfig          `yaml:"model"`
	StrongModel        StrongModelConfig    `yaml:"strong_model"`
	Limits             Limits               `yaml:"limits"`
	Timezone           string               `yaml:"timezone"`
	Quiet              QuietHours           `yaml:"quiet_hours"`
	DeliveryIntervalMS int                  `yaml:"delivery_interval_ms"`
	DefaultTarget      string               `yaml:"default_target"`
	Targets            map[string]Target    `yaml:"targets"`
	Permissions        map[string]string    `yaml:"permissions"`
	Actions            map[string]Operation `yaml:"actions"`
}
type ModelConfig struct {
	Endpoint       string `yaml:"endpoint"`
	Name           string `yaml:"name"`
	APIKey         string `yaml:"api_key"`
	MaxTokens      int    `yaml:"max_tokens"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}
// StrongModelConfig is the tier that runs critical actions: work whose output
// enters every later run. Enabling it consents to sending the records those
// actions read to this service. Requests use only standard chat-completions
// fields so hosted APIs accept them.
type StrongModelConfig struct {
	Enabled         bool `yaml:"enabled"`
	ModelConfig     `yaml:",inline"`
	MaxSteps        int `yaml:"max_steps"`
	RunSeconds      int `yaml:"run_seconds"`
	MaxContextBytes int `yaml:"max_context_bytes"`
}

type Limits struct {
	RunSeconds         int `yaml:"run_seconds"`
	ToolSeconds        int `yaml:"tool_seconds"`
	MaxSteps           int `yaml:"max_steps"`
	MaxInputBytes      int `yaml:"max_input_bytes"`
	MaxOutputBytes     int `yaml:"max_output_bytes"`
	MaxBatch           int `yaml:"max_batch"`
	MaxHistoryMessages int `yaml:"max_history_messages"`
	MaxRunsPerHour     int `yaml:"max_runs_per_hour"`
	RetryBaseMS        int `yaml:"retry_base_ms"`
	DebounceMS         int `yaml:"debounce_ms"`
	PollMS             int `yaml:"poll_ms"`
	ApprovalSeconds    int `yaml:"approval_seconds"`
}
type QuietHours struct {
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}
type Target struct {
	Kind  string `yaml:"kind"`
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}
type Operation struct {
	Mode        string `yaml:"mode"`
	URL         string `yaml:"url"`
	Method      string `yaml:"method"`
	BearerToken string `yaml:"bearer_token"`
}

func DefaultConfig() Config {
	return Config{WeCom: WeComConfig{ProgressMessages: true}, NewsFeeds: []NewsFeed{
		{"量子位", "https://www.qbitai.com/feed", "zh"},
		{"TechCrunch AI", "https://techcrunch.com/category/artificial-intelligence/feed/", "en"},
		{"Google AI", "https://blog.google/innovation-and-ai/technology/ai/rss/", "en"},
	}, Listen: "127.0.0.1:8090", Model: ModelConfig{Endpoint: "http://localhost:8080/v1/chat/completions", MaxTokens: 2048, TimeoutSeconds: 120}, StrongModel: StrongModelConfig{ModelConfig: ModelConfig{TimeoutSeconds: 300}, MaxSteps: 40, RunSeconds: 1800, MaxContextBytes: 512 << 10}, Limits: Limits{RunSeconds: 240, ToolSeconds: 15, MaxSteps: 12, MaxInputBytes: 16384, MaxOutputBytes: 16384, MaxBatch: 20, MaxHistoryMessages: 24, MaxRunsPerHour: 120, RetryBaseMS: 1000, DebounceMS: 1000, PollMS: 250, ApprovalSeconds: 3600}, Timezone: "UTC", DefaultTarget: "local", Targets: map[string]Target{"local": {Kind: "local"}}, Permissions: map[string]string{"read_file": "allow", "write_file": "allow", "edit_file": "allow", "set_timer": "allow", "send_message": "allow", "bash": "deny"}, Actions: map[string]Operation{}}
}
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	names := make(map[string]bool)
	for _, feed := range c.NewsFeeds {
		if feed.Name == "" || strings.ContainsAny(feed.Name, "\r\n") || names[feed.Name] || validateURL(feed.URL) != nil || !strings.HasPrefix(feed.URL, "https://") {
			return fmt.Errorf("invalid or duplicate news feed %q", feed.Name)
		}
		names[feed.Name] = true
	}
	switch c.provider() {
	case "dingtalk":
		if err := c.DingTalk.validate(); err != nil {
			return err
		}
	case "wecom":
		if err := c.WeCom.validate(); err != nil {
			return err
		}
	default:
		return errors.New("im.provider must be dingtalk or wecom")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("listen must use a loopback address; use an authenticated reverse proxy for remote access")
	}
	if _, e := time.LoadLocation(c.Timezone); e != nil {
		return e
	}
	if e := validateURL(c.Model.Endpoint); e != nil {
		return fmt.Errorf("model endpoint: %w", e)
	}
	if c.Model.MaxTokens < 1 || c.Model.TimeoutSeconds < 1 {
		return errors.New("invalid model limits")
	}
	if e := c.StrongModel.validate(); e != nil {
		return fmt.Errorf("strong_model: %w", e)
	}
	l := c.Limits
	if l.RunSeconds < 1 || l.ToolSeconds < 1 || l.MaxSteps < 1 || l.MaxSteps > 100 || l.MaxInputBytes < 1 || l.MaxInputBytes > 1048576 || l.MaxOutputBytes < 128 || l.MaxOutputBytes > 1048576 || l.MaxBatch < 1 || l.MaxBatch > 100 || l.MaxHistoryMessages < 0 || l.MaxHistoryMessages > 100 || l.MaxRunsPerHour < 1 || l.RetryBaseMS < 1 || l.DebounceMS < 0 || l.PollMS < 10 || l.ApprovalSeconds < 1 {
		return errors.New("invalid execution limits")
	}
	if c.DeliveryIntervalMS < 0 {
		return errors.New("invalid delivery interval")
	}
	if (c.Quiet.Start == "") != (c.Quiet.End == "") {
		return errors.New("quiet hours require start and end")
	}
	for _, v := range []string{c.Quiet.Start, c.Quiet.End} {
		if v != "" {
			if _, e := time.Parse("15:04", v); e != nil {
				return e
			}
		}
	}
	if _, ok := c.Targets[c.DefaultTarget]; !ok {
		return errors.New("default_target is not configured")
	}
	for _, t := range c.Targets {
		if t.Kind != "local" && t.Kind != "webhook" {
			return errors.New("target kind must be local or webhook")
		}
		if t.Kind == "webhook" {
			if e := validateURL(t.URL); e != nil {
				return e
			}
		}
	}
	for name, mode := range c.Permissions {
		switch name {
		case "bash", "read_file", "write_file", "edit_file", "set_timer", "send_message":
		default:
			return fmt.Errorf("unknown tool %q", name)
		}
		if mode != "allow" && mode != "deny" {
			return errors.New("base tools support allow/deny; put ask on a named external operation")
		}
	}
	for name, a := range c.Actions {
		if name == "" || (a.Mode != "allow" && a.Mode != "ask" && a.Mode != "deny") {
			return errors.New("invalid action policy")
		}
		if e := validateURL(a.URL); e != nil {
			return e
		}
		switch a.Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return errors.New("action method must be explicit")
		}
		if a.Mode == "ask" && a.Method == "GET" {
			return errors.New("GET queries should use allow or deny")
		}
	}
	return nil
}
func (c StrongModelConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if e := validateURL(c.Endpoint); e != nil {
		return fmt.Errorf("endpoint: %w", e)
	}
	u, _ := url.Parse(c.Endpoint)
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("endpoint must use https unless it is on this machine")
	}
	if c.Name == "" {
		return errors.New("name is required")
	}
	if c.MaxTokens < 0 || c.TimeoutSeconds < 1 || c.MaxSteps < 1 || c.MaxSteps > 200 || c.RunSeconds < 1 || c.MaxContextBytes < 1<<10 {
		return errors.New("invalid limits")
	}
	return nil
}

func validateURL(s string) error {
	u, e := url.Parse(s)
	if e != nil {
		return e
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("expected http(s) URL without userinfo or fragment")
	}
	return nil
}
