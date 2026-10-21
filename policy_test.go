package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

func stubConfirm(t *testing.T, answer bool, err error) *int {
	t.Helper()
	calls := 0
	old := confirm
	confirm = func(string, string) (bool, error) { calls++; return answer, err }
	t.Cleanup(func() { confirm = old })
	return &calls
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "100": 100, "2k": 2 << 10, "512M": 512 << 20, "3G": 3 << 30} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Fatalf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "G", "1.5G", "abc", "99999999999T"} {
		if _, err := parseSize(bad); err == nil {
			t.Fatalf("parseSize(%q) accepted", bad)
		}
	}
}

func TestAdmitTransferPolicy(t *testing.T) {
	dir := t.TempDir()
	calls := stubConfirm(t, true, nil)
	t.Setenv("TWO_O_CONFIRM_BYTES", "1M")

	t.Setenv("TWO_O_MAX_BYTES", "10M")
	if err := admitTransfer("big.iso", 11<<20, dir); err == nil || !strings.Contains(err.Error(), "TWO_O_MAX_BYTES") {
		t.Fatalf("hard cap not enforced: %v", err)
	}
	if *calls != 0 {
		t.Fatal("asked for confirmation of a transfer that the cap refuses anyway")
	}

	if err := admitTransfer("small.pdf", 1<<20, dir); err != nil || *calls != 0 {
		t.Fatalf("at-threshold transfer should pass silently: %v, %d prompts", err, *calls)
	}
	if err := admitTransfer("video.mp4", 2<<20, dir); err != nil || *calls != 1 {
		t.Fatalf("accepted confirmation: %v, %d prompts", err, *calls)
	}

	stubConfirm(t, false, nil)
	if err := admitTransfer("video.mp4", 2<<20, dir); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal not honoured: %v", err)
	}
	stubConfirm(t, false, errors.New("no notification daemon"))
	if err := admitTransfer("video.mp4", 2<<20, dir); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing prompt must refuse, not allow: %v", err)
	}

	t.Setenv("TWO_O_MAX_BYTES", "0")
	t.Setenv("TWO_O_CONFIRM_BYTES", "0")
	if err := admitTransfer("huge", 1<<50, dir); err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("free-space check missing: %v", err)
	}
}

// The remote must not stream a single payload byte when the desktop refuses.
func TestRefusedTransferSendsNoPayload(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("SSH_CONNECTION", "test")
	t.Setenv("TWO_O_CONFIRM_BYTES", "1k")
	stubConfirm(t, false, nil)
	startDesktop(t)
	src := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := open(src); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(cache, "2o"))
	if len(entries) != 0 {
		t.Fatalf("refused transfer left %d cache entries", len(entries))
	}

	stubConfirm(t, true, nil)
	t.Setenv("PATH", t.TempDir()) // no xdg-open: the copy lands, opening fails visibly
	err := open(src)
	if entries, _ = os.ReadDir(filepath.Join(cache, "2o")); len(entries) != 1 {
		t.Fatalf("accepted transfer should land in cache (err %v)", err)
	}
}

func TestClipboardLimitIsEnforcedByDesktop(t *testing.T) {
	t.Setenv("TWO_O_CLIPBOARD_BYTES", "8")
	startDesktop(t)
	c, err := request(wire.Message{Op: "copy", Size: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = response(c); err == nil || !strings.Contains(err.Error(), "clipboard too large") {
		t.Fatalf("oversized clipboard accepted: %v", err)
	}
}

func TestCacheCleanupKeepsRecentAndRecoveryCopies(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	for _, name := range []string{"2o-old", "2o-new", "edit-old"} {
		if err := os.MkdirAll(filepath.Join(dir, name, "f"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"2o-old", "edit-old"} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	cleanCache(dir, now)
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	if exists("2o-old") || !exists("2o-new") || !exists("edit-old") {
		t.Fatalf("cleanup result: old=%v new=%v recovery=%v", exists("2o-old"), exists("2o-new"), exists("edit-old"))
	}
	if err := os.Chtimes(filepath.Join(dir, "2o-new"), old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_CACHE_DAYS", "0")
	cleanCache(dir, now)
	if !exists("2o-new") {
		t.Fatal("TWO_O_CACHE_DAYS=0 must keep everything")
	}
}
