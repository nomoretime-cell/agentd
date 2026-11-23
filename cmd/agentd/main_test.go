package main

import (
	"os"
	"strings"
	"testing"
)

func TestLegacyLayoutStopsDefaultStart(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := checkLegacyLayout(false); err != nil {
		t.Fatal("empty directory rejected:", err)
	}
	if err := os.MkdirAll("data", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("data/state.db", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkLegacyLayout(false); err == nil || !strings.Contains(err.Error(), "-root .") {
		t.Fatalf("legacy layout not reported: %v", err)
	}
	if err := checkLegacyLayout(true); err != nil {
		t.Fatal("explicit -root rejected:", err)
	}
	if err := os.Mkdir(".agentd", 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkLegacyLayout(false); err != nil {
		t.Fatal("migrated layout rejected:", err)
	}
}
