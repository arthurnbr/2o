package desktop

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	go func() { done <- Edit(path) }()
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
