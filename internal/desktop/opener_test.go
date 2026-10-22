package desktop

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A machine that is both a desktop and a 2o server has ~/.local/bin/xdg-open
// -> 2o early on PATH. The daemon must skip it and use the system opener;
// using it would send the file back to 2o, forever.
func TestOpenSkipsTwoOShim(t *testing.T) {
	root := t.TempDir()
	shimDir, sysDir := filepath.Join(root, "local"), filepath.Join(root, "usr")
	for _, d := range []string{shimDir, sysDir} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	twoO := filepath.Join(shimDir, "2o")
	if err := os.WriteFile(twoO, []byte("#!/bin/sh\necho LOOP >> \"$OPEN_LOG\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("2o", filepath.Join(shimDir, "xdg-open")); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "log")
	if err := os.WriteFile(filepath.Join(sysDir, "xdg-open"), []byte("#!/bin/sh\necho \"system $1\" >> \"$OPEN_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_LOG", log)
	oldGetenv, oldGoos, oldLook := getenv, goos, lookPath
	t.Cleanup(func() { getenv, goos, lookPath = oldGetenv, oldGoos, oldLook })
	goos = "linux"
	lookPath = exec.LookPath
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+sysDir)
	getenv = os.Getenv

	if err := Open("/tmp/report.pdf"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "system /tmp/report.pdf" {
		t.Fatalf("opener log = %q, want only the system xdg-open", got)
	}

	// Only the shim on PATH: a clear error, not a loop.
	t.Setenv("PATH", shimDir)
	err := Open("/tmp/report.pdf")
	if err == nil || !strings.Contains(err.Error(), "only 2o's own xdg-open") {
		t.Fatalf("shim only: got %v", err)
	}
	if c := openerCheck(); c.OK || !strings.Contains(c.Detail, "install xdg-utils") {
		t.Fatalf("doctor must flag it: %+v", c)
	}
}
