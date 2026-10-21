// Package desktop runs local desktop integrations without a shell.
package desktop

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const maxOSC52Bytes = 100 * 1024

var (
	// ErrUnavailable means there is no supported desktop command for this environment.
	ErrUnavailable = errors.New("desktop integration unavailable")
	// ErrTooLarge means the clipboard payload exceeds the OSC 52 safety limit.
	ErrTooLarge = errors.New("OSC 52 clipboard payload too large")
)

// Open opens a URL or local path with the desktop's associated application.
func Open(target string) error {
	name := ""
	switch runtime.GOOS {
	case "linux":
		name = "xdg-open"
	case "darwin":
		name = "open"
	default:
		return fmt.Errorf("%w: open on %s", ErrUnavailable, runtime.GOOS)
	}
	if err := available(name); err != nil {
		return err
	}
	_, err := run(name, nil, false, target)
	return err
}

// Copy places data on the local system clipboard. There is no implicit terminal fallback.
func Copy(data []byte) error {
	name, args, err := clipboardCommand(true)
	if err != nil {
		return err
	}
	_, err = run(name, data, false, args...)
	return err
}

// Paste reads the local system clipboard.
func Paste() ([]byte, error) {
	name, args, err := clipboardCommand(false)
	if err != nil {
		return nil, err
	}
	return run(name, nil, true, args...)
}

func clipboardCommand(copying bool) (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		name := "pbpaste"
		if copying {
			name = "pbcopy"
		}
		return name, nil, available(name)
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			name := "wl-paste"
			args := []string{"--no-newline"}
			if copying {
				name, args = "wl-copy", nil
			}
			if available(name) == nil {
				return name, args, nil
			}
		}
		if os.Getenv("DISPLAY") != "" {
			if err := available("xclip"); err == nil {
				args := []string{"-selection", "clipboard", "-out"}
				if copying {
					args[2] = "-in"
				}
				return "xclip", args, nil
			}
		}
		return "", nil, fmt.Errorf("%w: no clipboard command for the active display", ErrUnavailable)
	default:
		return "", nil, fmt.Errorf("%w: clipboard on %s", ErrUnavailable, runtime.GOOS)
	}
}

// Notify displays a desktop notification; unsupported or headless sessions fail visibly.
func Notify(title, body string) error {
	switch runtime.GOOS {
	case "linux":
		if err := available("notify-send"); err != nil {
			return err
		}
		_, err := run("notify-send", nil, false, "--", title, body)
		return err
	case "darwin":
		if err := available("osascript"); err != nil {
			return err
		}
		// Arguments, rather than source interpolation, prevent AppleScript injection.
		_, err := run("osascript", nil, false,
			"-e", "on run argv",
			"-e", "display notification (item 3 of argv) with title (item 2 of argv)",
			"-e", "end run",
			"2o", title, body)
		return err
	default:
		return fmt.Errorf("%w: notifications on %s", ErrUnavailable, runtime.GOOS)
	}
}

// Edit blocks until a graphical editor closes the file. The daemon has no terminal.
// Editor priority is TWO_O_EDITOR, VISUAL, EDITOR, then a detected graphical editor.
func Edit(path string) error {
	editor := ""
	for _, key := range []string{"TWO_O_EDITOR", "VISUAL", "EDITOR"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			editor = value
			break
		}
	}
	if editor == "" {
		for _, candidate := range []string{"code --wait", "codium --wait", "gvim -f", "emacsclient -c -w"} {
			if _, err := exec.LookPath(strings.Fields(candidate)[0]); err == nil {
				editor = candidate
				break
			}
		}
	}
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return fmt.Errorf("%w: no editor configured", ErrUnavailable)
	}
	if err := available(fields[0]); err != nil {
		return err
	}
	cmd := exec.Command(fields[0], append(fields[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return commandError(fields[0], err, "")
	}
	return nil
}

// OSC52 writes an OSC 52 terminal clipboard sequence for at most 100 KiB of data.
// It does not select a clipboard fallback or write to a terminal implicitly.
func OSC52(data []byte, w io.Writer) error {
	if len(data) > maxOSC52Bytes {
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrTooLarge, len(data), maxOSC52Bytes)
	}
	if w == nil {
		return errors.New("nil OSC 52 writer")
	}
	if _, err := io.WriteString(w, "\x1b]52;c;"); err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	if _, err := encoder.Write(data); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\a")
	return err
}

func available(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, name, err)
	}
	return nil
}

func run(name string, input []byte, capture bool, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	if capture {
		cmd.Stdout = &stdout
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, commandError(name, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func commandError(name string, err error, stderr string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, name, err)
	}
	if message := strings.TrimSpace(stderr); message != "" {
		return fmt.Errorf("%s: %w: %s", name, err, message)
	}
	return fmt.Errorf("%s: %w", name, err)
}
