package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A new state root gets the shipped templates once; later edits are kept.
func TestTemplatesCopiedOnce(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	shipped, _ := defaults.ReadFile("defaults/IDENTITY.md")
	got, err := os.ReadFile(filepath.Join(root, "workspace/IDENTITY.md"))
	if err != nil || string(got) != string(shipped) {
		t.Fatalf("identity template not copied: %v", err)
	}
	if err = os.WriteFile(filepath.Join(root, "workspace/IDENTITY.md"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _ = os.ReadFile(filepath.Join(root, "workspace/IDENTITY.md")); string(got) != "edited\n" {
		t.Fatal("operator edit overwritten on restart")
	}
}

// Built-in skills follow the binary; an operator file with the same name wins,
// and neither can be written by the model.
func TestBuiltInSkillsServedFromBinary(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	const name = "skills/memory-consolidation/SKILL.md"
	if _, err := os.Stat(filepath.Join(s.root, "workspace", name)); !os.IsNotExist(err) {
		t.Fatal("built-in skill copied into the workspace")
	}
	b, err := s.workspaceRead(name)
	if err != nil || !strings.Contains(string(b), "name: memory-consolidation") {
		t.Fatalf("built-in skill not readable: %v", err)
	}
	if err = os.MkdirAll(filepath.Join(s.root, "workspace/skills/memory-consolidation"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.root, "workspace", name), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.workspaceRead(name); string(b) != "local" {
		t.Fatal("operator skill does not override the built-in one")
	}
	if err = s.workspaceWrite(name, "x", digest(b)); err == nil {
		t.Fatal("model wrote a skill")
	}
	if _, err = s.workspaceRead("skills/../../data/api-token"); err == nil {
		t.Fatal("path escaped the workspace")
	}
}

// opencode_binary is a program path: it resolves against the working
// directory, not the state root.
func TestOpenCodeBinaryRelativeToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	c := testConfig()
	c.OpenCodeBinary = "opencode"
	s, err := Open(filepath.Join(t.TempDir(), "state"), c, finalModel())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.cfg.OpenCodeBinary != filepath.Join(dir, "opencode") {
		t.Fatalf("resolved to %s", s.cfg.OpenCodeBinary)
	}
}
