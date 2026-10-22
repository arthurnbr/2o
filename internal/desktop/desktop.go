// Package desktop runs local desktop integrations without a shell.
package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const maxOSC52Bytes = 100 * 1024

var (
	// ErrUnavailable means there is no supported desktop command for this environment.
	ErrUnavailable = errors.New("desktop integration unavailable")
	// ErrTooLarge means content exceeds a size limit (clipboard capture, OSC 52).
	ErrTooLarge = errors.New("content too large")
	// ErrTimeout means nobody answered a confirmation prompt in time.
	ErrTimeout = errors.New("no answer in time")
)

// limitError is a size refusal whose message carries the context; it matches
// ErrTooLarge with errors.Is without repeating the sentinel's text.
type limitError struct{ msg string }

func (e limitError) Error() string      { return e.msg }
func (limitError) Is(target error) bool { return target == ErrTooLarge }

// openGrace is how long Open watches the opener before reporting success.
// xdg-open's generic mode (Hyprland, Sway, …) runs the viewer in the
// foreground and returns only when its window closes; real failures (no
// application for the type, missing file) exit within this delay.
var openGrace = 1500 * time.Millisecond

// Open opens a URL or local path with the desktop's associated application
// and returns once it is launched, not when the application exits: the
// opener keeps running detached in its own process group. A failure that
// happens within openGrace is reported.
func Open(target string) error {
	path, err := opener()
	if err != nil {
		return err
	}
	errFile, err := os.CreateTemp("", "2o-open-")
	if err != nil {
		return err
	}
	_ = os.Remove(errFile.Name())
	defer errFile.Close()
	cmd := exec.Command(path, target)
	cmd.Stderr = errFile // a file, not a pipe: children may keep it open
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return commandError(path, err, "")
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }() // reaps it whenever it ends
	select {
	case err := <-exited:
		if err == nil {
			return nil
		}
		_, _ = errFile.Seek(0, io.SeekStart)
		msg, _ := io.ReadAll(io.LimitReader(errFile, 4096))
		return commandError(path, err, string(msg))
	case <-time.After(openGrace):
		return nil // still running: the application is open
	}
}

// opener finds the system's xdg-open (open on macOS) on PATH, skipping any
// that is 2o itself: the `xdg-open -> 2o` shim 2o puts on servers would, on a
// machine that is both a desktop and a 2o server, hand files back to 2o,
// which sends them to this daemon again, forever.
func opener() (string, error) {
	name := "xdg-open"
	switch goos {
	case "linux":
	case "darwin":
		name = "open"
	default:
		return "", fmt.Errorf("%w: open on %s", ErrUnavailable, goos)
	}
	p, err := lookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not installed", ErrUnavailable, name)
	}
	if !isTwoO(p) {
		return p, nil
	}
	// The first one on PATH is 2o's shim: take the next real one.
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		c := filepath.Join(dir, name)
		st, err := os.Stat(c)
		if err == nil && !st.IsDir() && st.Mode()&0111 != 0 && !isTwoO(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: only 2o's own %s link was found on PATH; install xdg-utils", ErrUnavailable, name)
}

// isTwoO reports whether p is a 2o binary: the running one, or any file
// named 2o it links to (another 2o build installed elsewhere).
func isTwoO(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	if filepath.Base(real) == "2o" {
		return true
	}
	self, err := selfExecutable()
	if err != nil {
		return false
	}
	a, errA := os.Stat(real)
	b, errB := os.Stat(self)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

// selfExecutable is the running binary; replaced in tests.
var selfExecutable = os.Executable

// Notify displays a desktop notification; unsupported or headless sessions fail visibly.
func Notify(title, body string) error {
	switch runtime.GOOS {
	case "linux":
		if err := available("notify-send"); err != nil {
			return err
		}
		_, err := runTool(toolCall{name: "notify-send", args: []string{"--app-name=2o", "--", title, escapeMarkup(body)}, timeout: toolTimeout})
		return err
	case "darwin":
		if err := available("osascript"); err != nil {
			return err
		}
		// Arguments, rather than source interpolation, prevent AppleScript injection.
		_, err := runTool(toolCall{name: "osascript", timeout: toolTimeout, args: []string{
			"-e", "on run argv",
			"-e", "display notification (item 3 of argv) with title (item 2 of argv)",
			"-e", "end run",
			"2o", title, body}})
		return err
	default:
		return fmt.Errorf("%w: notifications on %s", ErrUnavailable, runtime.GOOS)
	}
}

// escapeMarkup neutralises the body markup that notification daemons
// interpret, so remote text is shown literally.
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ConfirmTimeout bounds how long a transfer waits for the user.
const ConfirmTimeout = 2 * time.Minute

var confirmWait = ConfirmTimeout // shortened in tests

// Confirm asks the desktop user a yes/no question. A refusal or a dismissed
// prompt returns false, nil; no answer within ConfirmTimeout returns an error
// wrapping ErrTimeout; any other error means no prompt could be shown.
func Confirm(title, body string) (bool, error) {
	var call toolCall
	switch runtime.GOOS {
	case "linux":
		if err := available("notify-send"); err != nil {
			return false, err
		}
		call = toolCall{name: "notify-send", args: []string{"--urgency=critical", "--app-name=2o",
			"--action=accept=Accept", "--action=refuse=Refuse", "--", title, escapeMarkup(body)}}
	case "darwin":
		if err := available("osascript"); err != nil {
			return false, err
		}
		call = toolCall{name: "osascript", args: []string{
			"-e", "on run argv",
			"-e", "set r to display dialog (item 2 of argv) with title (item 1 of argv) buttons {\"Refuse\", \"Accept\"} default button \"Refuse\" giving up after " + fmt.Sprint(int(confirmWait.Seconds())-10),
			"-e", "if gave up of r then return \"timeout\"",
			"-e", "return button returned of r",
			"-e", "end run", title, body}}
	default:
		return false, fmt.Errorf("%w: confirmation on %s", ErrUnavailable, runtime.GOOS)
	}
	call.capture, call.limit, call.timeout = true, 256, confirmWait
	out, err := runTool(call)
	answer := strings.ToLower(strings.TrimSpace(string(out)))
	switch {
	case errors.Is(err, context.DeadlineExceeded) || answer == "timeout":
		return false, fmt.Errorf("%w after %s", ErrTimeout, confirmWait)
	case err != nil && runtime.GOOS == "darwin":
		return false, nil // osascript exits non-zero when the dialog is cancelled
	case err != nil:
		return false, err // notify-send fails only when no notification daemon answers
	}
	return answer == "accept", nil
}

// OSC52 writes an OSC 52 terminal clipboard sequence for at most 100 KiB of data.
// It does not select a clipboard fallback or write to a terminal implicitly.
func OSC52(data []byte, w io.Writer) error {
	if len(data) > maxOSC52Bytes {
		return limitError{fmt.Sprintf("OSC 52 payload is %d bytes; terminals accept at most %d", len(data), maxOSC52Bytes)}
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
	if !has(name) {
		return fmt.Errorf("%w: %s is not installed", ErrUnavailable, name)
	}
	return nil
}

// toolTimeout bounds clipboard, notification and query tools, which answer
// in milliseconds when healthy; a hung one must not hold the daemon.
const toolTimeout = 15 * time.Second

// toolCall describes one run of a desktop tool.
type toolCall struct {
	name    string
	args    []string
	env     []string      // added to this process environment
	input   []byte        // stdin; nil means /dev/null
	capture bool          // return stdout, refusing more than limit bytes
	limit   int64         // capture bound
	timeout time.Duration // 0 waits as long as the tool runs (opener, editor)
}

// runCall runs a desktop tool. Tools such as wl-copy, xclip or a browser
// launched by xdg-open leave background children holding inherited fds, so
// non-captured output goes to /dev/null and stderr to a file: no pipe means
// Wait returns when the tool itself exits. Captured output is read with a
// hard bound: on overflow the tool's process group is killed and reaped
// before anything more is buffered. Bounded runs are killed the same way on
// timeout; a forked clipboard owner that outlives a successful tool is kept.
func runCall(c toolCall) ([]byte, error) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	if c.env != nil {
		cmd.Env = append(os.Environ(), c.env...)
	}
	if c.input != nil {
		cmd.Stdin = bytes.NewReader(c.input)
	}
	if c.capture || c.timeout > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	}
	errFile, err := os.CreateTemp("", "2o-stderr-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(errFile.Name())
	defer errFile.Close()
	cmd.Stderr = errFile
	var stdout io.Reader
	if c.capture {
		if stdout, err = cmd.StdoutPipe(); err != nil {
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, commandError(c.name, err, "")
	}
	var out []byte
	if c.capture {
		bound := c.limit
		if bound < math.MaxInt64 {
			bound++ // one byte past the limit proves overflow
		}
		out, _ = io.ReadAll(io.LimitReader(stdout, bound))
		if int64(len(out)) > c.limit {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return nil, limitError{fmt.Sprintf("%s output is larger than %d bytes", filepath.Base(c.name), c.limit)}
		}
	}
	err = cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s did not finish within %s: %w", filepath.Base(c.name), c.timeout, context.DeadlineExceeded)
	}
	if err != nil {
		_, _ = errFile.Seek(0, io.SeekStart)
		msg, _ := io.ReadAll(io.LimitReader(errFile, 4096))
		return nil, commandError(c.name, err, string(msg))
	}
	return out, nil
}

// commandError names the tool once: "xclip exited with status 1: <stderr>".
func commandError(name string, err error, stderr string) error {
	tool := filepath.Base(name)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s is not installed", ErrUnavailable, tool)
	}
	detail := strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(stderr), tool+":")), " ")
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.Exited():
		err = fmt.Errorf("%s exited with status %d", tool, exit.ExitCode())
	case errors.As(err, &exit):
		err = fmt.Errorf("%s was stopped (%s)", tool, exit.ProcessState)
	default:
		err = fmt.Errorf("%s: %w", tool, err)
	}
	if detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}
