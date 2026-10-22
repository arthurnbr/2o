package desktop

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Session describes how to reach a graphical session's clipboard. It is
// filled from the current environment, then from the systemd user manager
// (where Plasma, GNOME, Hyprland, Sway… publish their session variables),
// then from sockets found on disk. Values are only those needed by clipboard
// tools; nothing else is inherited from the session.
type Session struct {
	Wayland    string // WAYLAND_DISPLAY
	X11        string // DISPLAY
	XAuthority string // XAUTHORITY
	Desktop    string // XDG_CURRENT_DESKTOP, e.g. "KDE", "GNOME", "Hyprland"
	RuntimeDir string // XDG_RUNTIME_DIR: a relative WAYLAND_DISPLAY is resolved there
	Source     string // where the values came from, for messages
}

func (s Session) graphical() bool { return s.Wayland != "" || s.X11 != "" }

func (s Session) env() []string {
	var e []string
	for k, v := range map[string]string{"WAYLAND_DISPLAY": s.Wayland, "DISPLAY": s.X11, "XAUTHORITY": s.XAuthority, "XDG_RUNTIME_DIR": s.RuntimeDir} {
		if v != "" {
			e = append(e, k+"="+v)
		}
	}
	return e
}

func (s Session) is(desktop string) bool {
	for _, d := range strings.Split(s.Desktop, ":") {
		if strings.EqualFold(d, desktop) {
			return true
		}
	}
	return false
}

// describe summarises the session for diagnostics.
func (s Session) describe() string {
	var parts []string
	if s.Wayland != "" {
		parts = append(parts, "Wayland "+s.Wayland)
	}
	if s.X11 != "" {
		parts = append(parts, "X11 "+s.X11)
	}
	if s.Desktop != "" {
		parts = append(parts, s.Desktop)
	}
	return strings.Join(parts, ", ") + " (from " + s.Source + ")"
}

// unavailableError is a missing-integration failure whose message stands on
// its own; it matches ErrUnavailable with errors.Is.
type unavailableError struct{ msg string }

func (e unavailableError) Error() string      { return e.msg }
func (unavailableError) Is(target error) bool { return target == ErrUnavailable }

func unavailable(format string, a ...any) error {
	return unavailableError{fmt.Sprintf(format, a...)}
}

// seams for tests
var (
	getenv      = os.Getenv
	systemdEnv  = readSystemdEnv
	scanSockets = func() Session { return scanSessionSockets(os.Getenv("XDG_RUNTIME_DIR"), "/tmp/.X11-unix") }
	lookPath    = exec.LookPath
	runTool     = runCall
	goos        = runtime.GOOS
	myUID       = os.Getuid
)

// CurrentSession returns the session described by this process environment.
// Used by the desktop daemon, which runs inside the graphical session.
func CurrentSession() Session {
	return Session{Wayland: getenv("WAYLAND_DISPLAY"), X11: getenv("DISPLAY"), XAuthority: getenv("XAUTHORITY"), Desktop: getenv("XDG_CURRENT_DESKTOP"), RuntimeDir: getenv("XDG_RUNTIME_DIR"), Source: "environment"}
}

// FindSession locates this user's graphical session even from an SSH shell
// or a service, where WAYLAND_DISPLAY/DISPLAY are unset. Displays named by
// the environment or the systemd user session are trusted because this
// user's session published them; sockets found on disk must belong to us.
func FindSession() Session {
	if s := CurrentSession(); s.graphical() {
		return s
	}
	if s := systemdEnv(); s.graphical() {
		return s
	}
	return scanSockets()
}

func readSystemdEnv() Session {
	if !has("systemctl") {
		return Session{}
	}
	out, err := runTool(toolCall{name: "systemctl", args: []string{"--user", "show-environment"}, capture: true, limit: 1 << 20, timeout: toolTimeout})
	if err != nil {
		return Session{}
	}
	s := Session{Source: "systemd user session"}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "WAYLAND_DISPLAY":
			s.Wayland = v
		case "DISPLAY":
			s.X11 = v
		case "XAUTHORITY":
			s.XAuthority = v
		case "XDG_CURRENT_DESKTOP":
			s.Desktop = v
		case "XDG_RUNTIME_DIR":
			s.RuntimeDir = v
		}
	}
	return s
}

// scanSessionSockets is the last resort: only sockets owned by this user
// count, so clipboard content never goes to another user's X server (any
// local user can create /tmp/.X11-unix/X*). Lstat: a symlink is not a socket.
func scanSessionSockets(runtimeDir, x11Dir string) Session {
	s := Session{Source: "session sockets", RuntimeDir: runtimeDir}
	if runtimeDir != "" {
		matches, _ := filepath.Glob(filepath.Join(runtimeDir, "wayland-*"))
		for _, m := range matches {
			if ownSocket(m) {
				s.Wayland = filepath.Base(m)
				break
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(x11Dir, "X*"))
	for _, m := range matches {
		n := strings.TrimPrefix(filepath.Base(m), "X")
		if n != "" && strings.Trim(n, "0123456789") == "" && ownSocket(m) {
			s.X11 = ":" + n
			break
		}
	}
	if s.X11 != "" {
		if home, err := os.UserHomeDir(); err == nil {
			if _, err := os.Stat(filepath.Join(home, ".Xauthority")); err == nil {
				s.XAuthority = filepath.Join(home, ".Xauthority")
			}
		}
	}
	return s
}

func ownSocket(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == myUID()
}

// backend is one way to reach a clipboard. copy is nil for read-only backends.
type backend struct {
	name  string
	ready func(Session) bool
	copy  func(Session, []byte) error
	paste func(s Session, limit int64) ([]byte, error)
}

func has(tool string) bool { _, err := lookPath(tool); return err == nil }

func qdbus() string {
	for _, t := range []string{"qdbus6", "qdbus-qt6", "qdbus"} {
		if has(t) {
			return t
		}
	}
	return ""
}

// clip runs a clipboard tool with the session environment and a timeout.
func clip(s Session, name string, input []byte, args ...string) error {
	_, err := runTool(toolCall{name: name, args: args, env: s.env(), input: input, timeout: toolTimeout})
	return err
}

// clipRead captures a clipboard tool's output, killing it past limit bytes.
func clipRead(s Session, limit int64, name string, args ...string) ([]byte, error) {
	return runTool(toolCall{name: name, args: args, env: s.env(), capture: true, limit: limit, timeout: toolTimeout})
}

func backends() []backend {
	return []backend{
		{
			name:  "wl-clipboard",
			ready: func(s Session) bool { return s.Wayland != "" && has("wl-copy") && has("wl-paste") },
			copy:  func(s Session, b []byte) error { return clip(s, "wl-copy", b) },
			paste: func(s Session, n int64) ([]byte, error) { return clipRead(s, n, "wl-paste", "--no-newline") },
		},
		{
			// Plasma's clipboard service, for reading only. Its setter takes
			// the text as a D-Bus argument, and every CLI that could send it
			// (qdbus, busctl, gdbus, dbus-send) reads arguments from argv
			// only, where other local users can see them. Copying on Plasma 6
			// therefore relies on wl-copy (KWin implements ext-data-control)
			// or on xclip through XWayland.
			name:  "Klipper (KDE)",
			ready: func(s Session) bool { return s.is("KDE") && qdbus() != "" },
			paste: func(s Session, n int64) ([]byte, error) {
				return clipRead(s, n, qdbus(), "org.kde.klipper", "/klipper", "getClipboardContents")
			},
		},
		{
			// X11 sessions, and XWayland (GNOME syncs it with the Wayland clipboard).
			name:  "xclip",
			ready: func(s Session) bool { return s.X11 != "" && has("xclip") },
			copy:  func(s Session, b []byte) error { return clip(s, "xclip", b, "-selection", "clipboard", "-in") },
			paste: func(s Session, n int64) ([]byte, error) {
				return clipRead(s, n, "xclip", "-selection", "clipboard", "-out")
			},
		},
		{
			name:  "xsel",
			ready: func(s Session) bool { return s.X11 != "" && has("xsel") },
			copy:  func(s Session, b []byte) error { return clip(s, "xsel", b, "--clipboard", "--input") },
			paste: func(s Session, n int64) ([]byte, error) { return clipRead(s, n, "xsel", "--clipboard", "--output") },
		},
	}
}

func darwinBackend() backend {
	return backend{
		name:  "pbcopy",
		ready: func(Session) bool { return has("pbcopy") },
		copy:  func(s Session, b []byte) error { return clip(Session{}, "pbcopy", b) },
		paste: func(_ Session, n int64) ([]byte, error) { return clipRead(Session{}, n, "pbpaste") },
	}
}

func usableBackends(s Session) []backend {
	all := backends()
	if goos == "darwin" {
		all = []backend{darwinBackend()}
	}
	var ok []backend
	for _, b := range all {
		if b.ready(s) {
			ok = append(ok, b)
		}
	}
	return ok
}

const noToolHint = "no clipboard tool for this session; install wl-clipboard (Wayland) or xclip (X11/XWayland)"

func noSession() error {
	return unavailable("no graphical session found (checked the environment, the systemd user session and session sockets)")
}

// readbackSlack lets a backend append a newline (qdbus) without failing the
// readback bound; anything longer cannot match and is refused unread.
const readbackSlack = 64

// CopyTo writes data to the session clipboard and verifies it by reading it
// back: some compositors (GNOME/Mutter) let wl-copy exit successfully without
// owning the clipboard, so success is only reported when the content is there.
// It returns the backend that worked.
func CopyTo(s Session, data []byte) (string, error) {
	if goos != "darwin" && !s.graphical() {
		return "", noSession()
	}
	var tried []string
	writable := 0
	for _, b := range usableBackends(s) {
		if b.copy == nil {
			continue
		}
		writable++
		if err := b.copy(s, data); err != nil {
			tried = append(tried, err.Error())
			continue
		}
		got, err := b.paste(s, int64(len(data))+readbackSlack)
		switch {
		case err != nil:
			tried = append(tried, fmt.Sprintf("%s: clipboard verification failed: %v", b.name, err))
		case !bytes.Equal(bytes.TrimRight(got, "\n"), bytes.TrimRight(data, "\n")):
			tried = append(tried, b.name+": clipboard verification failed: the content read back differs")
		default:
			return b.name, nil
		}
	}
	if writable == 0 {
		return "", unavailable(noToolHint)
	}
	return "", fmt.Errorf("could not set the clipboard: %s", strings.Join(tried, "; "))
}

// PasteFrom reads the session clipboard with the first working backend,
// refusing content larger than limit bytes without buffering it.
func PasteFrom(s Session, limit int64) ([]byte, error) {
	if goos != "darwin" && !s.graphical() {
		return nil, noSession()
	}
	var tried []string
	for _, b := range usableBackends(s) {
		out, err := b.paste(s, limit)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, ErrTooLarge) {
			return nil, limitError{fmt.Sprintf("the clipboard holds more than %d bytes", limit)}
		}
		tried = append(tried, err.Error())
	}
	if len(tried) == 0 {
		return nil, unavailable(noToolHint)
	}
	return nil, fmt.Errorf("could not read the clipboard: %s", strings.Join(tried, "; "))
}

// Copy and PasteLimit serve the desktop daemon. A daemon started outside the
// graphical session (systemd, autostart) still finds it via FindSession.
func Copy(data []byte) error {
	_, err := CopyTo(FindSession(), data)
	return err
}

// PasteLimit returns the clipboard content, or an error matching ErrTooLarge
// when it holds more than max bytes.
func PasteLimit(max int64) ([]byte, error) { return PasteFrom(FindSession(), max) }
