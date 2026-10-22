package desktop

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOSC52BoundariesAndContent(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, maxOSC52Bytes} {
		data := bytes.Repeat([]byte{0, '\n', 0xff}, n/3+1)[:n]
		var buf bytes.Buffer
		if err := OSC52(data, &buf); err != nil {
			t.Fatalf("length %d: %v", n, err)
		}
		want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString(data) + "\a"
		if got := buf.String(); got != want {
			t.Fatalf("length %d: OSC 52 sequence differs", n)
		}
	}
	var buf bytes.Buffer
	if err := OSC52(make([]byte, maxOSC52Bytes+1), &buf); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit error = %v, want ErrTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatal("over-limit payload wrote to terminal")
	}
}

func TestOSC52WriterFailure(t *testing.T) {
	broken := errors.New("broken output")
	if err := OSC52([]byte("abc"), failWriter{broken}); !errors.Is(err, broken) {
		t.Fatalf("write error = %v, want %v", err, broken)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestEditBlocksAndUsesEditorPrecedence(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("editor test requires a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "editor")
	ready := filepath.Join(dir, "ready")
	gate := filepath.Join(dir, "gate")
	path := filepath.Join(dir, "name with spaces")
	content := "#!/bin/sh\n[ \"$1\" = '--wait' ] || exit 8\nprintf changed > \"$2\"\nprintf ready > \"$TWO_O_TEST_READY\"\nwhile [ ! -e \"$TWO_O_TEST_GATE\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_EDITOR", script+" --wait")
	t.Setenv("VISUAL", "nonexistent-visual-editor")
	t.Setenv("EDITOR", "nonexistent-editor")
	t.Setenv("TWO_O_TEST_READY", ready)
	t.Setenv("TWO_O_TEST_GATE", gate)
	defer os.WriteFile(gate, nil, 0600)
	done := make(chan error, 1)
	go func() { done <- Edit(path, 0) }()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("editor returned before writing ready marker: %v", err)
		case <-deadline:
			t.Fatal("editor did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		t.Fatalf("editor did not wait for process exit: %v", err)
	default:
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("editor did not finish")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "changed" {
		t.Fatalf("edited content = %q, error = %v", got, err)
	}
}

func TestOpenPassesTargetAsArgument(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported desktop")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(dir, "opened")
	t.Setenv("TWO_O_TEST_OPENED", marker)
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	writeScript(t, dir, name, "#!/bin/sh\nprintf '%s' \"$1\" > \"$TWO_O_TEST_OPENED\"\n")
	target := "https://example.org/path?x='; echo wrong"
	if err := Open(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != target {
		t.Fatalf("opened = %q, error = %v", got, err)
	}
}

func writeScript(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestEditorSelection(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		tools  []string
		want   string // joined command; "" means refused
		errHas string
	}{
		{"TWO_O_EDITOR wins over detection", map[string]string{"TWO_O_EDITOR": "my-ed --block", "EDITOR": "gvim"}, []string{"code", "my-ed"}, "my-ed --block", ""},
		{"detected GUI editor beats terminal EDITOR", map[string]string{"EDITOR": "vim", "VISUAL": "nvim"}, []string{"vim", "nvim", "kate"}, "kate --block", ""},
		{"detection order prefers code", nil, []string{"gedit", "code", "gvim"}, "code --wait", ""},
		{"known GUI VISUAL gets its blocking flag", map[string]string{"VISUAL": "subl"}, []string{"subl"}, "subl --wait", ""},
		{"flag not duplicated", map[string]string{"EDITOR": "subl --wait"}, []string{"subl"}, "subl --wait", ""},
		{"terminal TWO_O_EDITOR refused", map[string]string{"TWO_O_EDITOR": "nvim"}, []string{"nvim", "code"}, "", "needs a terminal"},
		{"emacs -nw refused", map[string]string{"TWO_O_EDITOR": "emacs -nw"}, []string{"emacs"}, "", "needs a terminal"},
		{"terminal-only EDITOR refused with hint", map[string]string{"EDITOR": "/usr/bin/vim", "VISUAL": "nano"}, []string{"vim", "nano"}, "", "VISUAL=nano needs a terminal; EDITOR=/usr/bin/vim needs a terminal"},
		{"unknown EDITOR refused", map[string]string{"EDITOR": "mystery"}, []string{"mystery"}, "", "not a known graphical editor"},
		{"nothing at all", nil, nil, "", "no graphical editor found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDesk{tools: map[string]bool{}}
			for _, tool := range c.tools {
				d.tools[tool] = true
				d.tools["/usr/bin/"+tool] = true
			}
			isolate(t, d, c.env, Session{}, Session{})
			got, err := resolveEditor()
			if c.want != "" {
				if err != nil || strings.Join(got, " ") != c.want {
					t.Fatalf("editor = %q, %v; want %q", got, err, c.want)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), c.errHas) || !strings.Contains(err.Error(), "TWO_O_EDITOR='code --wait'") {
				t.Fatalf("want refusal containing %q and the hint, got %q, %v", c.errHas, got, err)
			}
		})
	}
}

func TestEditArgsAndErrors(t *testing.T) {
	d := &fakeDesk{tools: map[string]bool{"code": true}, fail: map[string]bool{"code": true}}
	isolate(t, d, nil, Session{}, Session{})
	if err := Edit("/tmp/f", 12); err == nil || err.Error() != "code exited with status 1" {
		t.Fatalf("edit failure: %v", err)
	}
	if args := d.calls[0].args; !slices.Equal(args, []string{"--wait", "--goto", "/tmp/f:12"}) {
		t.Fatalf("code args %q", args)
	}
	isolate(t, &fakeDesk{}, map[string]string{"TWO_O_EDITOR": "/opt/ed/missing --wait"}, Session{}, Session{})
	if err := Edit("/tmp/f", 0); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "TWO_O_EDITOR='code --wait'") {
		t.Fatalf("missing editor: %v", err)
	}
}

func TestCommandErrorNamesToolOnce(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "xclip")
	writeScript(t, dir, "xclip", "#!/bin/sh\necho 'xclip: Error: Can not open display' >&2\nexit 1\n")
	_, err := runCall(toolCall{name: tool, capture: true, limit: 10, timeout: toolTimeout})
	if err == nil || err.Error() != "xclip exited with status 1: Error: Can not open display" {
		t.Fatalf("message: %v", err)
	}
	_, err = runCall(toolCall{name: filepath.Join(dir, "missing")})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing tool should be ErrUnavailable: %v", err)
	}
}

func TestConfirmTimeoutIsNotARefusal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("notify-send path")
	}
	dir := t.TempDir()
	writeScript(t, dir, "notify-send", "#!/bin/sh\nexec sleep 30\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := confirmWait
	confirmWait = 300 * time.Millisecond
	t.Cleanup(func() { confirmWait = old })
	ok, err := Confirm("t", "b")
	if ok || !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v, %v", ok, err)
	}
	writeScript(t, dir, "notify-send", "#!/bin/sh\necho refuse\n")
	if ok, err := Confirm("t", "b"); ok || err != nil {
		t.Fatalf("declined: %v, %v", ok, err)
	}
	writeScript(t, dir, "notify-send", "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s' \"$last\" > \"$0.body\"\necho accept\n")
	if ok, err := Confirm("t", "<b>a & b</b>"); !ok || err != nil {
		t.Fatalf("accepted: %v, %v", ok, err)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "notify-send.body")); string(body) != "&lt;b&gt;a &amp; b&lt;/b&gt;" {
		t.Fatalf("markup not escaped: %q", body)
	}
}

func TestDoctor(t *testing.T) {
	d := &fakeDesk{
		tools:  map[string]bool{"xdg-open": true, "wl-copy": true, "wl-paste": true, "qdbus6": true, "notify-send": true, "busctl": true, "kate": true},
		output: map[string]string{"busctl": `as 3 "body" "actions" "persistence"` + "\n"},
	}
	isolate(t, d, nil, Session{Wayland: "wayland-0", Desktop: "KDE", Source: "systemd user session"}, Session{})
	got := map[string]CheckResult{}
	for _, r := range Doctor() {
		got[r.Name] = r
	}
	want := map[string]string{
		"graphical session": "Wayland wayland-0, KDE (from systemd user session)",
		"opener":            "xdg-open",
		"clipboard":         "wl-clipboard, Klipper (KDE) (read only)",
		"notifications":     "notification daemon supports Accept/Refuse buttons",
		"editor":            "kate --block",
	}
	for name, detail := range want {
		if r := got[name]; !r.OK || r.Detail != detail {
			t.Errorf("%s = %+v, want OK %q", name, r, detail)
		}
	}
	for _, c := range d.calls {
		if c.name != "busctl" || c.input != nil {
			t.Errorf("doctor ran %s %v: only read-only queries allowed", c.name, c.args)
		}
	}

	// Headless, no buttons, terminal editor only.
	d = &fakeDesk{tools: map[string]bool{"notify-send": true, "gdbus": true, "vim": true}, output: map[string]string{"gdbus": "(['body'],)\n"}}
	isolate(t, d, map[string]string{"EDITOR": "vim"}, Session{}, Session{})
	got = map[string]CheckResult{}
	for _, r := range Doctor() {
		got[r.Name] = r
	}
	for _, name := range []string{"graphical session", "opener", "clipboard", "editor"} {
		if got[name].OK {
			t.Errorf("%s should fail: %+v", name, got[name])
		}
	}
	if r := got["notifications"]; !r.OK || !strings.Contains(r.Detail, "no buttons") {
		t.Errorf("notifications = %+v", r)
	}
	d.fail = map[string]bool{"gdbus": true}
	if r := notifyCheck(); r.OK || !strings.Contains(r.Detail, "no notification daemon") {
		t.Errorf("unreachable daemon = %+v", r)
	}
}
