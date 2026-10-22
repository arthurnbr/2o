package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurnbr/2o/internal/ui"
)

// runCLI runs the command line with stdout and the UI captured.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var ui_ bytes.Buffer
	ui.SetOutput(&ui_)
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	err = run(args)
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	ui.SetOutput(os.Stderr)
	if err != nil {
		code = exitCode(err)
		var ue *userError
		if errors.As(err, &ue) {
			ui_.WriteString("✗ " + ue.msg + "\n")
			for _, h := range ue.hints {
				ui_.WriteString("→ " + h + "\n")
			}
		} else {
			ui_.WriteString(err.Error())
		}
	}
	return string(out), ui_.String(), code
}

func TestHelpIsOnStdoutWithExitZero(t *testing.T) {
	t.Setenv("TWO_O_SESSION", "")
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		out, errOut, code := runCLI(t, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, "2o edit [-n] FILE[:LINE]") || !strings.Contains(out, "2o status [-v]") {
			t.Fatalf("%q: code %d stderr %q stdout %q", args, code, errOut, out)
		}
		for _, internal := range []string{"daemon", "shell"} {
			if strings.Contains(out, "2o "+internal) {
				t.Errorf("%q: internal command %s shown in help", args, internal)
			}
		}
	}
	for want, args := range map[string][]string{"edit": {"edit", "--help"}, "paste": {"paste", "-c", "--help"}, "ssh": {"ssh", "--help"}, "status": {"help", "status"}} {
		out, _, code := runCLI(t, args...)
		if code != 0 || !strings.HasPrefix(out, "Usage: 2o "+want) {
			t.Fatalf("%q: code %d stdout %q", args, code, out)
		}
	}
}

func TestVersion(t *testing.T) {
	out, _, code := runCLI(t, "--version")
	if code != 0 || !strings.HasPrefix(out, "2o "+version+" ") {
		t.Fatalf("code %d %q", code, out)
	}
}

func TestUnknownCommandSuggestsAndExitsUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	_, errOut, code := runCLI(t, "opne")
	if code != exitUsage || !strings.Contains(errOut, `unknown command "opne"`) || !strings.Contains(errOut, "did you mean: 2o open") || !strings.Contains(errOut, "2o --help") {
		t.Fatalf("code %d:\n%s", code, errOut)
	}
	_, errOut, code = runCLI(t, "zzzzzz", "x")
	if code != exitUsage || strings.Contains(errOut, "did you mean") {
		t.Fatalf("code %d:\n%s", code, errOut)
	}
	if _, _, code = runCLI(t, "--bogus"); code != exitUsage {
		t.Fatalf("unknown option exit %d", code)
	}
	// Internal commands are hidden, not suggested.
	if _, errOut, _ = runCLI(t, "deamon"); strings.Contains(errOut, "2o daemon") {
		t.Fatalf("suggested internal command:\n%s", errOut)
	}
}

// A single existing path is still opened (`2o report.pdf`); only names that
// are neither paths nor URLs are unknown commands.
func TestLonePathIsOpened(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("report", []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_CONNECTION", "test")
	t.Setenv("TWO_O_SESSION", filepath.Join(dir, "missing-session"))
	t.Setenv("TWO_O_DESKTOP", "")
	_, errOut, code := runCLI(t, "report")
	if strings.Contains(errOut, "unknown command") || code != exitNoTunnel {
		t.Fatalf("existing file not opened (code %d):\n%s", code, errOut)
	}
}

func TestArgumentValidation(t *testing.T) {
	t.Setenv("TWO_O_SESSION", "")
	for _, args := range [][]string{
		{"copy", "x"}, {"paste", "-x"}, {"paste", "file"}, {"notify", "only-title"}, {"open"}, {"open", "a", "b"},
		{"edit"}, {"edit", "a", "b"}, {"edit", "--force", "a"}, {"status", "now"}, {"doctor", "x"},
	} {
		_, errOut, code := runCLI(t, args...)
		if code != exitUsage || !strings.Contains(errOut, "usage: 2o "+args[0]) {
			t.Errorf("%q: code %d\n%s", args, code, errOut)
		}
	}
}

// `copy --help` must print help, not consume (and copy) stdin.
func TestCopyHelpDoesNotReadStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteString("secret"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	out, _, code := runCLI(t, "copy", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: 2o copy") {
		t.Fatalf("code %d %q", code, out)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "secret" {
		t.Fatalf("stdin consumed: %q left", rest)
	}
}

// Without a tunnel, copy may fall back to OSC 52 only on a terminal; into a
// pipe it must fail with exit 3 and write no escape codes.
func TestCopyWithoutTunnelNeverWritesEscapesToPipes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSH_CONNECTION", "test")
	t.Setenv("TWO_O_SESSION", filepath.Join(dir, "missing"))
	t.Setenv("TWO_O_DESKTOP", "")
	r, w, _ := os.Pipe()
	w.WriteString("data")
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	out, errOut, code := runCLI(t, "copy")
	if code != exitNoTunnel || out != "" || !strings.Contains(errOut, "OSC 52") {
		t.Fatalf("code %d stdout %q\n%s", code, out, errOut)
	}
}

func TestStatusShowsDesktopVersionAndAge(t *testing.T) {
	startDesktop(t)
	out, errOut, code := runCLI(t, "status")
	if code != 0 || out != "" {
		t.Fatalf("code %d stdout %q", code, out)
	}
	for _, want := range []string{"✓ connected to " + desktopLabel(), "desktop  " + desktopLabel(), "version  " + version, "session  less than a minute old"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("missing %q in:\n%s", want, errOut)
		}
	}
	ui.SetQuiet(true)
	t.Cleanup(func() { ui.SetQuiet(false) })
	if _, errOut, code = runCLI(t, "status"); code != 0 || errOut != "" {
		t.Fatalf("quiet status: code %d %q", code, errOut)
	}
}

func TestStatusWithoutTunnelExitsNoTunnel(t *testing.T) {
	t.Setenv("TWO_O_SESSION", "")
	if _, _, code := runCLI(t, "status"); code != exitNoTunnel {
		t.Fatalf("exit %d", code)
	}
}

func TestDoctorReportsDesktopChecks(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no desktop tools: checks fail, nothing is touched
	startDesktop(t)
	t.Setenv("TWO_O_MAX_BYTES", "2G")
	_, errOut, _ := runCLI(t, "status", "-v")
	for _, want := range []string{"✓ session", "✓ tunnel  connected to " + desktopLabel(), "desktop 2o", "transfer policy  max 2 GiB", "editor"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("missing %q in:\n%s", want, errOut)
		}
	}
}

func TestDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"opne", "open", 2}, {"", "abc", 3}, {"edit", "edit", 0}, {"stauts", "status", 2}, {"pste", "paste", 1}} {
		if got := distance(c.a, c.b); got != c.d {
			t.Errorf("distance(%q, %q) = %d, want %d", c.a, c.b, got, c.d)
		}
	}
}
