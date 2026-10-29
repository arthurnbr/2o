package desktop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DownloadDir is where received downloads go: $TWO_O_DOWNLOAD_DIR, else the
// XDG download directory (xdg-user-dir DOWNLOAD), else ~/Downloads. A
// leading ~/ is expanded. The directory is created if missing.
func DownloadDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(getenv("TWO_O_DOWNLOAD_DIR"))
	if dir == "" {
		dir = xdgDownload(home)
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok || dir == "~" {
		dir = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("TWO_O_DOWNLOAD_DIR must be an absolute path, not %q", dir)
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// xdgDownload asks xdg-user-dir (which also reads user-dirs.dirs) and falls
// back to ~/Downloads. xdg-user-dir prints $HOME when nothing is set.
func xdgDownload(home string) string {
	if has("xdg-user-dir") {
		out, err := runTool(toolCall{name: "xdg-user-dir", args: []string{"DOWNLOAD"}, capture: true, limit: 4096, timeout: toolTimeout})
		if d := strings.TrimSpace(string(out)); err == nil && d != "" && filepath.Clean(d) != filepath.Clean(home) {
			return d
		}
	}
	return filepath.Join(home, "Downloads")
}

// PickTimeout bounds how long a file picker waits for the user.
const PickTimeout = 5 * time.Minute

var pickWait = PickTimeout // shortened in tests

// ErrCancelled means the user closed the picker without choosing.
var ErrCancelled = errors.New("no file chosen")

// Pick shows a file picker on the desktop and returns the chosen files (or,
// with dirs, folders), multiple allowed. Closing it returns ErrCancelled;
// no answer within PickTimeout an error wrapping ErrTimeout.
func Pick(title string, dirs bool) ([]string, error) {
	call, err := pickerCall(title, dirs)
	if err != nil {
		return nil, err
	}
	call.capture, call.limit, call.timeout = true, 1<<20, pickWait
	out, err := runTool(call)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("%w after %s", ErrTimeout, pickWait)
	case err != nil && strings.Contains(err.Error(), "exited with status 1"):
		// zenity, kdialog and osascript all exit 1 when cancelled.
		return nil, ErrCancelled
	case err != nil:
		return nil, err
	}
	var picked []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			picked = append(picked, p)
		}
	}
	if len(picked) == 0 {
		return nil, ErrCancelled
	}
	return picked, nil
}

func pickerCall(title string, dirs bool) (toolCall, error) {
	switch goos {
	case "linux":
		s := FindSession()
		if !s.graphical() {
			return toolCall{}, noSession()
		}
		env := s.env()
		switch {
		case has("zenity"):
			args := []string{"--file-selection", "--multiple", "--separator=\n", "--title=" + title}
			if dirs {
				args = append(args, "--directory")
			}
			return toolCall{name: "zenity", args: args, env: env}, nil
		case has("kdialog"):
			mode := "--getopenfilename"
			if dirs {
				mode = "--getexistingdirectory"
			}
			return toolCall{name: "kdialog", args: []string{"--title", title, mode, ".", "--multiple", "--separate-output"}, env: env}, nil
		}
		return toolCall{}, unavailable("no file picker; install zenity (GTK) or kdialog (KDE)")
	case "darwin":
		if err := available("osascript"); err != nil {
			return toolCall{}, err
		}
		kind := "file"
		if dirs {
			kind = "folder"
		}
		return toolCall{name: "osascript", args: []string{
			"-e", "on run argv",
			"-e", "set l to choose " + kind + " with prompt (item 1 of argv) with multiple selections allowed",
			"-e", "set out to \"\"",
			"-e", "repeat with f in l",
			"-e", "set out to out & POSIX path of f & linefeed",
			"-e", "end repeat",
			"-e", "return out",
			"-e", "end run", title}}, nil
	}
	return toolCall{}, fmt.Errorf("%w: file picker on %s", ErrUnavailable, goos)
}

// pickerCheck reports, for doctor, which file picker upload would use.
func pickerCheck() CheckResult {
	call, err := pickerCall("", false)
	if err != nil {
		return CheckResult{Name: "file picker", Detail: strings.TrimPrefix(err.Error(), ErrUnavailable.Error()+": ")}
	}
	return CheckResult{Name: "file picker", OK: true, Detail: call.name}
}
