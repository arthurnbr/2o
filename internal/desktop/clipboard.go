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

// seams for tests
var (
	getenv      = os.Getenv
	systemdEnv  = readSystemdEnv
	scanSockets = func() Session { return scanSessionSockets(os.Getenv("XDG_RUNTIME_DIR"), "/tmp/.X11-unix") }
	lookPath    = exec.LookPath
	runTool     = func(name string, env []string, input []byte, capture bool, args ...string) ([]byte, error) {
		return runEnv(name, env, input, capture, args...)
	}
)

// CurrentSession returns the session described by this process environment.
// Used by the desktop daemon, which runs inside the graphical session.
func CurrentSession() Session {
	return Session{Wayland: getenv("WAYLAND_DISPLAY"), X11: getenv("DISPLAY"), XAuthority: getenv("XAUTHORITY"), Desktop: getenv("XDG_CURRENT_DESKTOP"), RuntimeDir: getenv("XDG_RUNTIME_DIR"), Source: "environment"}
}

// FindSession locates this user's graphical session even from an SSH shell
// or a service, where WAYLAND_DISPLAY/DISPLAY are unset.
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
	if _, err := lookPath("systemctl"); err != nil {
		return Session{}
	}
	out, err := runTool("systemctl", nil, nil, true, "--user", "show-environment")
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

func scanSessionSockets(runtimeDir, x11Dir string) Session {
	s := Session{Source: "session sockets", RuntimeDir: runtimeDir}
	if runtimeDir != "" {
		matches, _ := filepath.Glob(filepath.Join(runtimeDir, "wayland-*"))
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && st.Mode()&os.ModeSocket != 0 && ownedByMe(st) {
				s.Wayland = filepath.Base(m)
				break
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(x11Dir, "X*"))
	for _, m := range matches {
		n := strings.TrimPrefix(filepath.Base(m), "X")
		if st, err := os.Stat(m); err == nil && st.Mode()&os.ModeSocket != 0 && n != "" && strings.Trim(n, "0123456789") == "" {
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

func ownedByMe(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

// backend is one way to reach a clipboard.
type backend struct {
	name  string
	ready func(Session) bool
	copy  func(Session, []byte) error
	paste func(Session) ([]byte, error)
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

func backends() []backend {
	return []backend{
		{
			name:  "wl-clipboard",
			ready: func(s Session) bool { return s.Wayland != "" && has("wl-copy") && has("wl-paste") },
			copy: func(s Session, b []byte) error {
				_, err := runTool("wl-copy", s.env(), b, false)
				return err
			},
			paste: func(s Session) ([]byte, error) { return runTool("wl-paste", s.env(), nil, true, "--no-newline") },
		},
		{
			// Plasma's clipboard service; works even where wl-copy is too old
			// for KWin's ext-data-control. Text only.
			name:  "Klipper (KDE)",
			ready: func(s Session) bool { return s.is("KDE") && qdbus() != "" },
			copy: func(s Session, b []byte) error {
				if bytes.IndexByte(b, 0) >= 0 {
					return errors.New("Klipper only accepts text")
				}
				_, err := runTool(qdbus(), s.env(), nil, false, "org.kde.klipper", "/klipper", "setClipboardContents", string(b))
				return err
			},
			paste: func(s Session) ([]byte, error) {
				return runTool(qdbus(), s.env(), nil, true, "org.kde.klipper", "/klipper", "getClipboardContents")
			},
		},
		{
			// X11 sessions, and XWayland (GNOME syncs it with the Wayland clipboard).
			name:  "xclip",
			ready: func(s Session) bool { return s.X11 != "" && has("xclip") },
			copy: func(s Session, b []byte) error {
				_, err := runTool("xclip", s.env(), b, false, "-selection", "clipboard", "-in")
				return err
			},
			paste: func(s Session) ([]byte, error) {
				return runTool("xclip", s.env(), nil, true, "-selection", "clipboard", "-out")
			},
		},
		{
			name:  "xsel",
			ready: func(s Session) bool { return s.X11 != "" && has("xsel") },
			copy: func(s Session, b []byte) error {
				_, err := runTool("xsel", s.env(), b, false, "--clipboard", "--input")
				return err
			},
			paste: func(s Session) ([]byte, error) { return runTool("xsel", s.env(), nil, true, "--clipboard", "--output") },
		},
	}
}

func darwinBackend() backend {
	return backend{
		name:  "pbcopy",
		ready: func(Session) bool { return has("pbcopy") },
		copy: func(_ Session, b []byte) error {
			_, err := runTool("pbcopy", nil, b, false)
			return err
		},
		paste: func(Session) ([]byte, error) { return runTool("pbpaste", nil, nil, true) },
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

var goos = runtime.GOOS

// CopyTo writes data to the session clipboard and verifies it by reading it
// back: some compositors (GNOME/Mutter) let wl-copy exit successfully without
// owning the clipboard, so success is only reported when the content is there.
// It returns the backend that worked.
func CopyTo(s Session, data []byte) (string, error) {
	if goos != "darwin" && !s.graphical() {
		return "", fmt.Errorf("%w: no graphical session found (looked in environment, systemd user session, sockets)", ErrUnavailable)
	}
	candidates := usableBackends(s)
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: no clipboard tool for this session; install wl-clipboard (Wayland) or xclip (X11/XWayland)", ErrUnavailable)
	}
	var tried []string
	for _, b := range candidates {
		if err := b.copy(s, data); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", b.name, err))
			continue
		}
		got, err := b.paste(s)
		if err == nil && bytes.Equal(bytes.TrimRight(got, "\n"), bytes.TrimRight(data, "\n")) {
			return b.name, nil
		}
		tried = append(tried, b.name+": content did not stick (compositor refused clipboard access)")
	}
	return "", fmt.Errorf("could not set clipboard: %s", strings.Join(tried, "; "))
}

// PasteFrom reads the session clipboard with the first working backend.
func PasteFrom(s Session) ([]byte, error) {
	if goos != "darwin" && !s.graphical() {
		return nil, fmt.Errorf("%w: no graphical session found", ErrUnavailable)
	}
	var tried []string
	for _, b := range usableBackends(s) {
		out, err := b.paste(s)
		if err == nil {
			return out, nil
		}
		tried = append(tried, fmt.Sprintf("%s: %v", b.name, err))
	}
	if len(tried) == 0 {
		return nil, fmt.Errorf("%w: no clipboard tool for this session; install wl-clipboard (Wayland) or xclip (X11/XWayland)", ErrUnavailable)
	}
	return nil, fmt.Errorf("could not read clipboard: %s", strings.Join(tried, "; "))
}

// Copy and Paste serve the desktop daemon. A daemon started outside the
// graphical session (systemd, autostart) still finds it via FindSession.
func Copy(data []byte) error {
	_, err := CopyTo(FindSession(), data)
	return err
}

func Paste() ([]byte, error) { return PasteFrom(FindSession()) }
