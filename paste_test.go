package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paste -c fetches the desktop clipboard through the tunnel and hands it to
// the server-side store; plain paste still writes to stdout.
func TestPasteClipboardModeStoresOnServer(t *testing.T) {
	bin := t.TempDir()
	clip := filepath.Join(bin, "desk-clip")
	if err := os.WriteFile(clip, []byte("from the desktop"), 0600); err != nil {
		t.Fatal(err)
	}
	// Desktop side: an X session whose xclip reads a file.
	script := "#!/bin/sh\ncase \"$3\" in -out) /bin/cat \"" + clip + "\";; -in) /bin/cat > \"" + clip + "\";; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "xclip"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":5")
	startDesktop(t)

	var stored []byte
	old := storeOnServer
	storeOnServer = func(b []byte) (string, error) { stored = b; return "test store", nil }
	t.Cleanup(func() { storeOnServer = old })

	if err := run([]string{"paste", "-c"}); err != nil {
		t.Fatal(err)
	}
	if string(stored) != "from the desktop" {
		t.Fatalf("stored %q", stored)
	}

	storeOnServer = func([]byte) (string, error) { return "", errors.New("must not be called") }
	r, w, _ := os.Pipe()
	oldOut := os.Stdout
	os.Stdout = w
	err := run([]string{"paste"})
	os.Stdout = oldOut
	w.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(r)
	if err != nil || out.String() != "from the desktop" {
		t.Fatalf("plain paste = %q, %v", out.String(), err)
	}
	if err := run([]string{"paste", "--bogus"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("unknown flag accepted: %v", err)
	}
}

func TestStoreOnServerFallsBackToTmux(t *testing.T) {
	bin := t.TempDir()
	buf := filepath.Join(bin, "tmux-buffer")
	tmux := "#!/bin/sh\n[ \"$1\" = load-buffer ] && [ \"$2\" = - ] && /bin/cat > \"" + buf + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmux), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin) // no systemctl, no clipboard tools
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	t.Setenv("TMUX", "")
	if _, err := storeOnServer([]byte("x")); err == nil || !strings.Contains(err.Error(), "2o paste > FILE") {
		t.Fatalf("headless without tmux should explain alternatives: %v", err)
	}
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	where, err := storeOnServer([]byte("into tmux"))
	if err != nil || !strings.Contains(where, "tmux") {
		t.Fatalf("tmux fallback: %q, %v", where, err)
	}
	if got, _ := os.ReadFile(buf); string(got) != "into tmux" {
		t.Fatalf("tmux buffer %q", got)
	}
}
