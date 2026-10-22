package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arthurnbr/2o/internal/desktop"
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

// stubDisk pins the free space admitTransfer sees, independent of the host.
func stubDisk(t *testing.T, free int64) {
	t.Helper()
	old := statfs
	statfs = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { statfs = old })
}

// policyEnv pins every TWO_O_* variable admission depends on.
func policyEnv(t *testing.T, maxBytes, confirmBytes string) {
	t.Setenv("TWO_O_MAX_BYTES", maxBytes)
	t.Setenv("TWO_O_CONFIRM_BYTES", confirmBytes)
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var ue *userError
	if !errors.As(err, &ue) || exitCode(err) != exitRefused {
		t.Fatalf("want a refusal (*userError, exit %d), got %T %v", exitRefused, err, err)
	}
	return err.Error()
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
	stubDisk(t, 100<<30)
	calls := stubConfirm(t, true, nil)
	policyEnv(t, "2G", "1M")

	_, err := admitTransfer("report.iso", 3<<30+1<<28, dir, nil)
	if msg := refusal(t, err); msg != "report.iso is 3.2 GiB, above this desktop's limit of 2 GiB (TWO_O_MAX_BYTES)" {
		t.Fatalf("cap message: %q", msg)
	}
	if *calls != 0 {
		t.Fatal("asked for confirmation of a transfer that the cap refuses anyway")
	}

	waits := 0
	onWait := func() { waits++ }
	release, err := admitTransfer("small.pdf", 1<<20, dir, onWait)
	if err != nil || *calls != 0 || waits != 0 {
		t.Fatalf("at-threshold transfer should pass silently: %v, %d prompts, %d waits", err, *calls, waits)
	}
	release()
	release, err = admitTransfer("video.mp4", 2<<20, dir, onWait)
	if err != nil || *calls != 1 || waits != 1 {
		t.Fatalf("accepted confirmation: %v, %d prompts, %d waits", err, *calls, waits)
	}
	release()

	stubConfirm(t, false, nil)
	_, err = admitTransfer("video.mp4", 2<<20, dir, nil)
	if msg := refusal(t, err); msg != "video.mp4 (2 MiB) was declined on the desktop" {
		t.Fatalf("declined message: %q", msg)
	}
	stubConfirm(t, false, fmt.Errorf("%w after 2m0s", desktop.ErrTimeout))
	_, err = admitTransfer("video.mp4", 2<<20, dir, nil)
	if msg := refusal(t, err); !strings.Contains(msg, "nobody answered") || strings.Contains(msg, "declined") {
		t.Fatalf("timeout must not read as a refusal: %q", msg)
	}
	stubConfirm(t, false, errors.New("no notification daemon"))
	_, err = admitTransfer("video.mp4", 2<<20, dir, nil)
	if msg := refusal(t, err); !strings.Contains(msg, "no prompt could be shown") || !strings.Contains(msg, "TWO_O_CONFIRM_BYTES") {
		t.Fatalf("missing prompt must refuse, not allow: %q", msg)
	}
	if inFlight.bytes != 0 {
		t.Fatalf("refused confirmations kept %d bytes reserved", inFlight.bytes)
	}

	policyEnv(t, "0", "0")
	stubDisk(t, 5<<30)
	_, err = admitTransfer("huge", 4<<30+1, dir, nil)
	if msg := refusal(t, err); msg != "huge is 4 GiB, but the desktop only has 5 GiB free (keeping 1 GiB spare)" {
		t.Fatalf("free-space message: %q", msg)
	}
	if release, err = admitTransfer("fits", 4<<30, dir, nil); err != nil {
		t.Fatalf("exactly free minus reserve must fit: %v", err)
	}
	release()
}

// Remote names are validated before anyone is prompted, and shown sanitized.
func TestAdmitTransferNames(t *testing.T) {
	stubDisk(t, 100<<30)
	policyEnv(t, "0", "1k")
	calls := stubConfirm(t, true, nil)
	for _, bad := range []string{"", "..", "a/b", "evil\x1b[2J.txt", "\xff.txt"} {
		if _, err := admitTransfer(bad, 2<<10, t.TempDir(), nil); err == nil {
			t.Errorf("accepted unsafe name %q", bad)
		} else if msg := refusal(t, err); strings.Contains(msg, "\x1b") || !utf8.ValidString(msg) {
			t.Errorf("refusal echoes raw bytes: %q", msg)
		}
	}
	if *calls != 0 {
		t.Fatalf("prompted %d times for names that are refused anyway", *calls)
	}
	var body string
	confirm = func(_, b string) (bool, error) { body = b; return true, nil }
	release, err := admitTransfer("pay\u202eslip   <i>ok&.pdf", 2<<10, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if body != "Receive payslip <i>ok&.pdf (2 KiB) from the SSH server?" {
		t.Fatalf("prompt body: %q", body)
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"  report\n\n2026.pdf ":  "report 2026.pdf",
		"a\x00b\x7fc\u200bd":     "abcd",
		"\u202egpj.exe":          "gpj.exe",
		"\t\n":                   "unnamed file",
		strings.Repeat("x", 100): strings.Repeat("x", maxDisplayName) + "…",
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Concurrent transfers must not each see the same free space: together they
// stay within free minus the reserve and within TWO_O_MAX_BYTES.
func TestConcurrentReservations(t *testing.T) {
	stubConfirm(t, true, nil)
	for _, tc := range []struct {
		name, max string
		free      int64
	}{
		{"disk", "0", 1<<30 + 10<<20}, // 10 MiB usable
		{"cap", "10M", 100 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policyEnv(t, tc.max, "0")
			stubDisk(t, tc.free)
			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				admitted int
				releases []func()
			)
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					release, err := admitTransfer("part.bin", 3<<20, os.TempDir(), nil)
					if err != nil {
						return
					}
					mu.Lock()
					admitted++
					releases = append(releases, release)
					mu.Unlock()
				}()
			}
			wg.Wait()
			if admitted != 3 {
				t.Fatalf("admitted %d × 3 MiB into 10 MiB", admitted)
			}
			if _, err := admitTransfer("more.bin", 3<<20, t.TempDir(), nil); !strings.Contains(refusal(t, err), "already being received") {
				t.Fatalf("refusal should mention in-flight bytes: %v", err)
			}
			for _, r := range releases {
				r()
				r() // idempotent
			}
			if inFlight.bytes != 0 {
				t.Fatalf("%d bytes still reserved after release", inFlight.bytes)
			}
			release, err := admitTransfer("again.bin", 9<<20, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("released space not reusable: %v", err)
			}
			release()
		})
	}
}

// The remote must not stream a single payload byte when the desktop refuses.
func TestRefusedTransferSendsNoPayload(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("SSH_CONNECTION", "test")
	policyEnv(t, "0", "1k")
	stubDisk(t, 100<<30)
	stubConfirm(t, false, nil)
	startDesktop(t)
	src := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := open(src); err == nil || !strings.Contains(err.Error(), "declined") {
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
	if err = response(c); err == nil || !strings.Contains(err.Error(), "accepts up to 8 B") {
		t.Fatalf("oversized clipboard accepted: %v", err)
	}
}

func TestCacheCleanupKeepsRecentAndRecoveryCopies(t *testing.T) {
	t.Setenv("TWO_O_CACHE_DAYS", "") // default 7 days, whatever the user's environment says
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
