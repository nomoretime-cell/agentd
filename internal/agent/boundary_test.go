package agent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Core modules must not know any specific IM platform or business (AGENTS.md,
// "架构不变量" 2). Channels and businesses plug in through im.go and their own
// files; a match here means a rule leaked into the core.
func TestCoreKnowsNoChannelOrBusiness(t *testing.T) {
	core := []string{"inbox.go", "timers.go", "outbox.go", "permissions.go", "sandbox.go", "memory.go", "consolidation.go", "search.go", "gate.go", "loop.go", "store.go", "service.go"}
	forbidden := regexp.MustCompile(`(?i:dingtalk|wecom)|news|News|钉钉|企微|企业微信|新闻|资讯`)
	for _, name := range core {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d mentions %q; move it behind im.go or the business file", name, i+1, m)
			}
		}
	}
}
