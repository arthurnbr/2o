package desktop

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOpener puts an executable xdg-open with the given script first on PATH.
func fakeOpener(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xdg-open"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	oldGetenv, oldGoos, oldLook, oldGrace := getenv, goos, lookPath, openGrace
	t.Cleanup(func() { getenv, goos, lookPath, openGrace = oldGetenv, oldGoos, oldLook, oldGrace })
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	getenv, goos, lookPath = os.Getenv, "linux", exec.LookPath
	openGrace = 300 * time.Millisecond
}

// On Hyprland/Sway xdg-open runs the viewer in the foreground until its
// window closes. Open must report success as soon as it is launched, so the
// remote `2o open` returns and the user can open the next file.
func TestOpenDoesNotWaitForTheViewerToClose(t *testing.T) {
	fakeOpener(t, "#!/bin/sh\nsleep 30\n")
	start := time.Now()
	if err := Open("/tmp/report.pdf"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Open waited %v for the viewer", took)
	}
}

// A real failure (no application, missing file) still comes back.
func TestOpenReportsQuickFailure(t *testing.T) {
	fakeOpener(t, "#!/bin/sh\necho 'no application for this file type' >&2\nexit 3\n")
	err := Open("/tmp/report.xyz")
	if err == nil || !strings.Contains(err.Error(), "exited with status 3") || !strings.Contains(err.Error(), "no application") {
		t.Fatalf("got %v", err)
	}
}

// A detaching opener (GNOME, KDE: exits 0 at once) is a success.
func TestOpenQuickSuccess(t *testing.T) {
	fakeOpener(t, "#!/bin/sh\nexit 0\n")
	start := time.Now()
	if err := Open("/tmp/report.pdf"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("a quick opener must not wait for the grace period")
	}
}
