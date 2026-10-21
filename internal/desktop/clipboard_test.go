package desktop

import (
	"bytes"
	"errors"
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
	calls     []string
	envs      map[string][]string
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
	runTool = func(name string, e []string, input []byte, capture bool, args ...string) ([]byte, error) {
		d.calls = append(d.calls, name+" "+strings.Join(args, " "))
		d.envs[name] = e
		if d.fail[name] {
			return nil, errors.New(name + " failed")
		}
		reading := capture || (name == "wl-paste") || (len(args) > 0 && (args[len(args)-1] == "-out" || args[len(args)-1] == "--output" || args[len(args)-1] == "getClipboardContents"))
		if reading {
			return append([]byte(nil), d.clipboard...), nil
		}
		if d.refuse[name] {
			return nil, nil
		}
		if name == "qdbus6" {
			d.clipboard = []byte(args[len(args)-1])
		} else {
			d.clipboard = append([]byte(nil), input...)
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

func TestKlipperUsedOnPlasmaWhenWlCopyFails(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"wl-copy": true, "wl-paste": true, "qdbus6": true}, fail: map[string]bool{"wl-copy": true}}
	isolate(t, d, nil, Session{}, Session{})
	b, err := CopyTo(Session{Wayland: "wayland-0", Desktop: "KDE"}, []byte("from plasma"))
	if err != nil || b != "Klipper (KDE)" || string(d.clipboard) != "from plasma" {
		t.Fatalf("Klipper fallback: %q, %v, clip %q", b, err, d.clipboard)
	}
	// Klipper is text-only and must not be used for binary data.
	if _, err := CopyTo(Session{Wayland: "wayland-0", Desktop: "KDE"}, []byte{'a', 0, 'b'}); err == nil {
		t.Fatal("binary data reported as copied through Klipper")
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
	if _, err := CopyTo(Session{Wayland: "wayland-0"}, []byte("x")); err == nil || !strings.Contains(err.Error(), "did not stick") {
		t.Fatalf("silent failure must be reported: %v", err)
	}
}

func TestPasteRoundTripBinary(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"xclip": true}}
	isolate(t, d, nil, Session{}, Session{X11: ":1"})
	data := []byte{'a', 0, '\n', 0xff}
	if err := Copy(data); err != nil {
		t.Fatal(err)
	}
	got, err := Paste()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("paste %q, %v", got, err)
	}
}

func TestScanSessionSockets(t *testing.T) {
	rt, x := t.TempDir(), t.TempDir()
	for _, p := range []string{filepath.Join(rt, "wayland-3"), filepath.Join(x, "X7")} {
		l, err := netListenUnix(p)
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

// Regression: wl-copy forks a background owner that keeps inherited fds open;
// waiting on a stdout/stderr pipe used to hang forever.
func TestToolLeavingBackgroundChildDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "forker")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n(sleep 30) &\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runEnv(tool, nil, []byte("x"), false); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runEnv waited for the background child")
	}
}

func netListenUnix(p string) (interface{ Close() error }, error) { return net.Listen("unix", p) }
