package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDesk simulates clipboard tools. Each tool name maps to behaviour; the
// shared "clipboard" is what a working tool would store.
type fakeDesk struct {
	tools     map[string]bool
	refuse    map[string]bool // copy succeeds but content does not stick (GNOME + wl-copy)
	fail      map[string]bool // tool exits with an error
	clipboard []byte
	calls     []toolCall
	envs      map[string][]string
	output    map[string]string // canned stdout for query tools (busctl…)
}

func isolate(t *testing.T, d *fakeDesk, env map[string]string, systemd Session, sockets Session) {
	t.Helper()
	oldGetenv, oldSystemd, oldScan, oldLook, oldRun, oldGoos := getenv, systemdEnv, scanSockets, lookPath, runTool, goos
	t.Cleanup(func() {
		getenv, systemdEnv, scanSockets, lookPath, runTool, goos = oldGetenv, oldSystemd, oldScan, oldLook, oldRun, oldGoos
	})
	goos = "linux"
	getenv = func(k string) string { return env[k] }
	systemdEnv = func() Session { return systemd }
	scanSockets = func() Session { return sockets }
	lookPath = func(n string) (string, error) {
		if d.tools[n] {
			return "/fake/" + n, nil
		}
		return "", exec.ErrNotFound
	}
	if d.envs == nil {
		d.envs = map[string][]string{}
	}
	runTool = func(c toolCall) ([]byte, error) {
		d.calls = append(d.calls, c)
		d.envs[c.name] = c.env
		if d.fail[c.name] {
			return nil, errors.New(c.name + " exited with status 1")
		}
		if out, ok := d.output[c.name]; ok {
			return []byte(out), nil
		}
		if c.capture {
			if int64(len(d.clipboard)) > c.limit {
				return nil, limitError{"too much"}
			}
			return append([]byte(nil), d.clipboard...), nil
		}
		if !d.refuse[c.name] {
			d.clipboard = append([]byte(nil), c.input...)
		}
		return nil, nil
	}
}

func TestFindSessionPrefersEnvThenSystemdThenSockets(t *testing.T) {
	d := &fakeDesk{}
	sd := Session{Wayland: "wayland-1", Desktop: "KDE", Source: "systemd user session"}
	sock := Session{X11: ":0", Source: "session sockets"}

	isolate(t, d, map[string]string{"WAYLAND_DISPLAY": "wayland-9"}, sd, sock)
	if s := FindSession(); s.Wayland != "wayland-9" || s.Source != "environment" {
		t.Fatalf("environment must win: %+v", s)
	}
	isolate(t, d, nil, sd, sock)
	if s := FindSession(); s.Wayland != "wayland-1" || s.Desktop != "KDE" {
		t.Fatalf("SSH shell should use systemd session: %+v", s)
	}
	isolate(t, d, nil, Session{}, sock)
	if s := FindSession(); s.X11 != ":0" {
		t.Fatalf("socket scan fallback: %+v", s)
	}
}

func TestCopyPassesSessionEnvToTool(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"wl-copy": true, "wl-paste": true}}
	isolate(t, d, nil, Session{}, Session{})
	s := Session{Wayland: "wayland-1", X11: ":0", XAuthority: "/run/user/1000/xauth"}
	if b, err := CopyTo(s, []byte("hi")); err != nil || b != "wl-clipboard" {
		t.Fatalf("CopyTo = %q, %v", b, err)
	}
	env := strings.Join(d.envs["wl-copy"], " ")
	for _, want := range []string{"WAYLAND_DISPLAY=wayland-1", "DISPLAY=:0", "XAUTHORITY=/run/user/1000/xauth"} {
		if !strings.Contains(env, want) {
			t.Fatalf("tool env %q lacks %s", env, want)
		}
	}
}

// GNOME/Mutter: wl-copy "succeeds" but nothing is stored; XWayland xclip works.
func TestCopyFallsBackWhenContentDoesNotStick(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"wl-copy": true, "wl-paste": true, "xclip": true}, refuse: map[string]bool{"wl-copy": true}}
	isolate(t, d, nil, Session{}, Session{})
	b, err := CopyTo(Session{Wayland: "wayland-0", X11: ":0", Desktop: "GNOME"}, []byte("secret text"))
	if err != nil || b != "xclip" {
		t.Fatalf("expected xclip fallback, got %q, %v (calls %v)", b, err, d.calls)
	}
}

// The readback after a copy is bounded near the copied size: a clipboard
// that suddenly holds much more is not buffered to compare it.
func TestCopyReadbackIsBounded(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"wl-copy": true, "wl-paste": true}, refuse: map[string]bool{"wl-copy": true}, clipboard: bytes.Repeat([]byte("x"), 1<<20)}
	isolate(t, d, nil, Session{}, Session{})
	_, err := CopyTo(Session{Wayland: "wayland-0"}, []byte("short"))
	if err == nil || !strings.Contains(err.Error(), "clipboard verification failed") {
		t.Fatalf("oversized readback: %v", err)
	}
	for _, c := range d.calls {
		if c.capture && c.limit > int64(len("short"))+readbackSlack {
			t.Fatalf("readback limit %d for 5 bytes", c.limit)
		}
	}
}

// Klipper's setter would need the text in argv (visible to every local
// user): on Plasma, copying must go through stdin-fed tools or fail.
func TestKlipperNeverReceivesContentInArgv(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"wl-copy": true, "wl-paste": true, "qdbus6": true, "xclip": true}, fail: map[string]bool{"wl-copy": true}}
	isolate(t, d, nil, Session{}, Session{})
	secret := "hunter2-secret"
	b, err := CopyTo(Session{Wayland: "wayland-0", X11: ":1", Desktop: "KDE"}, []byte(secret))
	if err != nil || b != "xclip" {
		t.Fatalf("Plasma copy should use XWayland xclip: %q, %v", b, err)
	}
	for _, c := range d.calls {
		if strings.Contains(strings.Join(c.args, " "), secret) {
			t.Fatalf("clipboard content passed in argv: %s %v", c.name, c.args)
		}
	}
	// Without a stdin-capable tool, copy fails rather than use Klipper.
	d.tools = map[string]bool{"qdbus6": true}
	if _, err := CopyTo(Session{Wayland: "wayland-0", Desktop: "KDE"}, []byte(secret)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Klipper-only copy: %v", err)
	}
	// Klipper still serves reads.
	d.clipboard = []byte("from klipper")
	if got, err := PasteFrom(Session{Wayland: "wayland-0", Desktop: "KDE"}, 100); err != nil || string(got) != "from klipper" {
		t.Fatalf("Klipper paste: %q, %v", got, err)
	}
}

func TestErrorsAreActionable(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{}}
	isolate(t, d, nil, Session{}, Session{})
	if _, err := CopyTo(Session{}, []byte("x")); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no graphical session") {
		t.Fatalf("headless: %v", err)
	}
	if _, err := CopyTo(Session{Wayland: "wayland-0"}, []byte("x")); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "install wl-clipboard") {
		t.Fatalf("missing tools: %v", err)
	}
	d.tools = map[string]bool{"wl-copy": true, "wl-paste": true}
	d.refuse = map[string]bool{"wl-copy": true}
	d.clipboard = []byte("older")
	_, err := CopyTo(Session{Wayland: "wayland-0"}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "clipboard verification failed: the content read back differs") || strings.Contains(err.Error(), "compositor") {
		t.Fatalf("silent failure must be reported without guessing the cause: %v", err)
	}
	d.refuse = nil
	d.fail = map[string]bool{"wl-copy": true}
	if _, err := CopyTo(Session{Wayland: "wayland-0"}, []byte("x")); err == nil || strings.Count(err.Error(), "wl-copy") != 1 {
		t.Fatalf("tool must be named once: %v", err)
	}
}

func TestPasteLimitRefusesLargeClipboard(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"xclip": true}, clipboard: []byte("123456789")}
	isolate(t, d, nil, Session{}, Session{X11: ":1"})
	if _, err := PasteLimit(8); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "more than 8 bytes") {
		t.Fatalf("over limit: %v", err)
	}
	if got, err := PasteLimit(9); err != nil || string(got) != "123456789" {
		t.Fatalf("at limit: %q, %v", got, err)
	}
}

func TestPasteRoundTripBinary(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"xclip": true}}
	isolate(t, d, nil, Session{}, Session{X11: ":1"})
	data := []byte{'a', 0, '\n', 0xff}
	if err := Copy(data); err != nil {
		t.Fatal(err)
	}
	got, err := PasteLimit(16)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("paste %q, %v", got, err)
	}
}

func TestScanSessionSockets(t *testing.T) {
	rt, x := t.TempDir(), t.TempDir()
	for _, p := range []string{filepath.Join(rt, "wayland-3"), filepath.Join(x, "X7")} {
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
	}
	if err := os.WriteFile(filepath.Join(x, "X1-lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := scanSessionSockets(rt, x)
	if s.Wayland != "wayland-3" || s.X11 != ":7" {
		t.Fatalf("scan = %+v", s)
	}
}

// Any local user can create /tmp/.X11-unix/X*: a socket that is not ours,
// or a symlink, must never receive the clipboard.
func TestScanIgnoresForeignX11Sockets(t *testing.T) {
	x := t.TempDir()
	l, err := net.Listen("unix", filepath.Join(x, "X0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.Symlink(filepath.Join(x, "X0"), filepath.Join(x, "X5")); err != nil {
		t.Fatal(err)
	}
	old := myUID
	t.Cleanup(func() { myUID = old })
	myUID = func() int { return os.Getuid() + 1 } // X0 now belongs to someone else
	if s := scanSessionSockets("", x); s.X11 != "" {
		t.Fatalf("foreign X server picked: %+v", s)
	}
	myUID = old
	if err := os.Remove(filepath.Join(x, "X0")); err != nil {
		t.Fatal(err)
	}
	l2, err := net.Listen("unix", filepath.Join(x, "X0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if s := scanSessionSockets("", x); s.X11 != ":0" {
		t.Fatalf("own X server (and not the symlink :5) expected: %+v", s)
	}
}

// Regression: wl-copy forks a background owner that keeps inherited fds open;
// waiting on a stdout/stderr pipe used to hang forever.
func TestToolLeavingBackgroundChildDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "forker")
	pidFile := filepath.Join(dir, "pid")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nsleep 30 &\necho $! > \""+pidFile+"\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPidFile(pidFile) })
	done := make(chan error, 1)
	go func() { _, err := runCall(toolCall{name: tool, input: []byte("x"), timeout: toolTimeout}); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCall waited for the background child")
	}
}

// A tool that never stops writing is killed as soon as it passes the limit.
func TestBoundedCaptureKillsEndlessProducer(t *testing.T) {
	yes, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("no yes(1)")
	}
	start := time.Now()
	_, err = runCall(toolCall{name: yes, capture: true, limit: 1 << 16, timeout: 30 * time.Second})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("endless output: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("overflow took %s to stop", d)
	}
}

// Clipboard and notification tools are killed when they hang.
func TestToolTimeoutKillsHungTool(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "hang")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := runCall(toolCall{name: tool, capture: true, limit: 10, timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("hung tool: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func killPidFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &pid); err == nil && pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}
