package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurnbr/2o/internal/ui"
)

// editSetup runs a real desktop daemon whose editor is the given shell script
// and returns the server-side directory and the desktop cache.
func editSetup(t *testing.T, script string) (dir, cache string, out *bytes.Buffer) {
	t.Helper()
	dir = t.TempDir()
	cache = filepath.Join(dir, "cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	policyEnv(t, "0", "0")
	stubDisk(t, 100<<30)
	editor := filepath.Join(dir, "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_EDITOR", editor)
	t.Setenv("SSH_CONNECTION", "loopback")
	out = &bytes.Buffer{}
	ui.SetOutput(out)
	t.Cleanup(func() { ui.SetOutput(os.Stderr) })
	startDesktop(t)
	return dir, cache, out
}

// recoveryPath extracts the desktop path from "your edits are safe in PATH on DESKTOP".
func recoveryPath(t *testing.T, err error) string {
	t.Helper()
	var ue *userError
	if !errors.As(err, &ue) {
		t.Fatalf("not a userError: %v", err)
	}
	for _, h := range ue.hints {
		if rest, ok := strings.CutPrefix(h, "your edits are safe in "); ok {
			if i := strings.LastIndex(rest, " on "); i > 0 {
				return rest[:i]
			}
		}
	}
	t.Fatalf("no recovery path in %q %q", ue.msg, ue.hints)
	return ""
}

func editsKept(t *testing.T, cache string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(cache, "2o", "edit-*", "*"))
	return m
}

func TestEditConflictKeepsRecoveryCopy(t *testing.T) {
	dir, _, _ := editSetup(t, "printf 'someone else changed it' > \"$TWO_O_TEST_REMOTE\"\nprintf 'my edited copy' > \"$1\"\n")
	remote := filepath.Join(dir, "original.txt")
	if err := os.WriteFile(remote, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_TEST_REMOTE", remote)
	err := editFile(remote, 0, false)
	if err == nil || !strings.Contains(err.Error(), "changed on the server while you were editing") {
		t.Fatalf("expected conflict, got %v", err)
	}
	path := recoveryPath(t, err)
	if b, e := os.ReadFile(path); e != nil || string(b) != "my edited copy" {
		t.Fatalf("recovery content %q: %v", b, e)
	}
	if b, _ := os.ReadFile(remote); string(b) != "someone else changed it" {
		t.Fatalf("server file overwritten: %q", b)
	}
}

// REVIEW P0 #2: the editor saves, then exits non-zero. The saved work must
// survive on the desktop and the error must say where.
func TestEditorFailureAfterSaveKeepsEdits(t *testing.T) {
	dir, cache, _ := editSetup(t, "printf 'saved before crash' > \"$1\"\nexit 1\n")
	remote := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(remote, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	err := editFile(remote, 0, false)
	if err == nil || !strings.Contains(err.Error(), "editor failed") {
		t.Fatalf("expected editor failure, got %v", err)
	}
	path := recoveryPath(t, err)
	if b, e := os.ReadFile(path); e != nil || string(b) != "saved before crash" {
		t.Fatalf("recovery content %q: %v", b, e)
	}
	if kept := editsKept(t, cache); len(kept) != 1 || kept[0] != path {
		t.Fatalf("kept copies %q, want [%s]", kept, path)
	}
	if b, _ := os.ReadFile(remote); string(b) != "before" {
		t.Fatalf("failed edit must not touch the server file: %q", b)
	}
}

// An editor that fails without writing leaves nothing to recover: no copy
// lingers and no recovery path is promised.
func TestEditorFailureWithoutChangesLeavesNothing(t *testing.T) {
	dir, cache, _ := editSetup(t, "exit 3\n")
	remote := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(remote, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	err := editFile(remote, 0, false)
	var ue *userError
	if !errors.As(err, &ue) || !strings.Contains(ue.msg, "was not changed") || strings.Contains(strings.Join(ue.hints, "|"), "safe in") {
		t.Fatalf("got %v", err)
	}
	if kept := editsKept(t, cache); len(kept) != 0 {
		t.Fatalf("unchanged copy kept: %q", kept)
	}
}

func TestEditSuccessAppliesAndCleansUp(t *testing.T) {
	dir, cache, out := editSetup(t, "printf 'new text' > \"$1\"\n")
	remote := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(remote, []byte("old text"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := editFile(remote, 0, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(remote); string(b) != "new text" {
		t.Fatalf("server file %q", b)
	}
	if st, _ := os.Stat(remote); st.Mode().Perm() != 0640 {
		t.Fatalf("mode %v not preserved", st.Mode().Perm())
	}
	if kept := editsKept(t, cache); len(kept) != 0 {
		t.Fatalf("committed edit left copies %q", kept)
	}
	if s := out.String(); !strings.Contains(s, "editing notes.txt on "+desktopLabel()+" — save and close the editor to apply") || !strings.Contains(s, "✓ notes.txt updated") {
		t.Fatalf("progress output:\n%s", s)
	}
}

func TestEditNewFile(t *testing.T) {
	dir, cache, _ := editSetup(t, "printf 'fresh' > \"$1\"\n")
	remote := filepath.Join(dir, "new.txt")
	err := editFile(remote, 0, false)
	var ue *userError
	if !errors.As(err, &ue) || ue.code != exitUsage || !strings.Contains(strings.Join(ue.hints, "|"), "2o edit -n") {
		t.Fatalf("missing file without -n: %v", err)
	}
	if err = editFile(remote, 0, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(remote); string(b) != "fresh" {
		t.Fatalf("created content %q", b)
	}
	if kept := editsKept(t, cache); len(kept) != 0 {
		t.Fatalf("created edit left copies %q", kept)
	}
}

// Creating must not clobber a file that appeared while the editor was open.
func TestEditNewFileRaceKeepsEdits(t *testing.T) {
	dir, _, _ := editSetup(t, "printf 'theirs' > \"$TWO_O_TEST_REMOTE\"\nprintf 'mine' > \"$1\"\n")
	remote := filepath.Join(dir, "new.txt")
	t.Setenv("TWO_O_TEST_REMOTE", remote)
	err := editFile(remote, 0, true)
	if err == nil || !strings.Contains(err.Error(), "was created on the server") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(recoveryPath(t, err)); string(b) != "mine" {
		t.Fatalf("recovery %q", b)
	}
	if b, _ := os.ReadFile(remote); string(b) != "theirs" {
		t.Fatalf("clobbered: %q", b)
	}
}

func TestEditTargetLine(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "odd:7")
	if err := os.WriteFile(odd, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]struct {
		path string
		line int
	}{
		"a.go:42": {"a.go", 42}, "a.go": {"a.go", 0}, "a.go:": {"a.go:", 0}, "a.go:x1": {"a.go:x1", 0},
		odd: {odd, 0}, odd + ":3": {odd, 3}, ":5": {":5", 0},
	} {
		if p, l := editTarget(in); p != want.path || l != want.line {
			t.Errorf("editTarget(%q) = %q, %d; want %q, %d", in, p, l, want.path, want.line)
		}
	}
}
