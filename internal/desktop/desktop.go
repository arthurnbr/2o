// Package desktop runs local desktop integrations without a shell.
package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
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

// confirmTimeout bounds how long a transfer waits for the user.
const confirmTimeout = 2 * time.Minute

// Confirm asks the desktop user a yes/no question. Dismissing or ignoring the
// prompt counts as no. An error means no prompt could be shown at all.
func Confirm(title, body string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		if err := available("notify-send"); err != nil {
			return false, err
		}
		cmd = exec.CommandContext(ctx, "notify-send", "--urgency=critical", "--app-name=2o",
			"--action=accept=Accept", "--action=refuse=Refuse", "--", title, body)
	case "darwin":
		if err := available("osascript"); err != nil {
			return false, err
		}
		cmd = exec.CommandContext(ctx, "osascript",
			"-e", "on run argv",
			"-e", "display dialog (item 2 of argv) with title (item 1 of argv) buttons {\"Refuse\", \"Accept\"} default button \"Refuse\" giving up after 110",
			"-e", "return button returned of result",
			"-e", "end run", title, body)
	default:
		return false, fmt.Errorf("%w: confirmation on %s", ErrUnavailable, runtime.GOOS)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return false, nil // nobody answered
	}
	answer := strings.ToLower(strings.TrimSpace(string(out)))
	if err != nil && answer == "" {
		// osascript exits non-zero when the dialog is cancelled; notify-send
		// only fails when it cannot reach a notification daemon.
		if runtime.GOOS == "darwin" {
			return false, nil
		}
		return false, commandError(cmd.Path, err, stderr.String())
	}
	return answer == "accept", nil
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
	return runEnv(name, nil, input, capture, args...)
}

// runEnv runs a desktop tool. Tools such as wl-copy, xclip or a browser
// launched by xdg-open leave background children holding inherited fds, so
// non-captured output goes to /dev/null and stderr to a file: no pipe means
// Wait returns when the tool itself exits.
func runEnv(name string, env []string, input []byte, capture bool, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	errFile, err := os.CreateTemp("", "2o-stderr-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(errFile.Name())
	defer errFile.Close()
	cmd.Stderr = errFile
	var stdout bytes.Buffer
	if capture {
		cmd.Stdout = &stdout
	}
	if err := cmd.Run(); err != nil {
		_, _ = errFile.Seek(0, io.SeekStart)
		msg, _ := io.ReadAll(io.LimitReader(errFile, 4096))
		return nil, commandError(name, err, string(msg))
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
