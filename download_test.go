package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurnbr/2o/internal/desktop"
)

// transferSetup runs a real desktop daemon whose download folder is
// returned; the "server" end saves uploads in its own folder.
func transferSetup(t *testing.T) (desk, server string) {
	t.Helper()
	root := t.TempDir()
	desk = filepath.Join(root, "desk-downloads")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("TWO_O_DOWNLOAD_DIR", desk)
	t.Setenv("SSH_CONNECTION", "loopback")
	policyEnv(t, "0", "0")
	stubDisk(t, 100<<30)
	captureUI(t)
	startDesktop(t)
	// Both ends of the test are this process: the daemon reads the same
	// TWO_O_DOWNLOAD_DIR, so uploads land in desk too.
	return desk, desk
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDownloadSavesWithoutReplacing(t *testing.T) {
	desk, _ := transferSetup(t)
	src := t.TempDir()
	// A script and an executable: download saves, it never opens, so they go.
	p := filepath.Join(src, "build.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(desk, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(desk, "build.sh"), []byte("mine"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := download([]string{p, p}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(desk, "build.sh")); got != "mine" {
		t.Fatalf("existing file replaced: %q", got)
	}
	for _, n := range []string{"build (1).sh", "build (2).sh"} {
		got := filepath.Join(desk, n)
		if read(t, got) != "#!/bin/sh\necho hi\n" {
			t.Fatalf("%s content differs", n)
		}
		if st, _ := os.Stat(got); st.Mode().Perm()&0111 != 0 {
			t.Fatalf("%s saved executable: %v", n, st.Mode())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(desk, ".2o-partial-*")); len(left) != 0 {
		t.Fatalf("partial transfers left: %q", left)
	}
}

func TestDownloadDirectory(t *testing.T) {
	desk, _ := transferSetup(t)
	src := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("A"), 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := download([]string{src}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"project", "project (1)"} {
		if read(t, filepath.Join(desk, d, "sub", "a.txt")) != "A" {
			t.Fatalf("%s not received", d)
		}
	}
}

func TestDownloadMissingFileIsUsageError(t *testing.T) {
	transferSetup(t)
	err := download([]string{filepath.Join(t.TempDir(), "nope")})
	if exitCode(err) != exitUsage {
		t.Fatalf("got %v", err)
	}
}

func stubPicker(t *testing.T, picked []string, err error) {
	t.Helper()
	old := pickFiles
	pickFiles = func(string, bool) ([]string, error) { return picked, err }
	t.Cleanup(func() { pickFiles = old })
}

func TestUploadSavesPickedFiles(t *testing.T) {
	_, server := transferSetup(t)
	src := t.TempDir()
	a, b := filepath.Join(src, "a.pdf"), filepath.Join(src, "b.txt")
	_ = os.WriteFile(a, []byte("%PDF"), 0644)
	_ = os.WriteFile(b, []byte("bee"), 0644)
	stubPicker(t, []string{a, b}, nil)
	if err := upload(false); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(server, "a.pdf")) != "%PDF" || read(t, filepath.Join(server, "b.txt")) != "bee" {
		t.Fatal("uploaded content differs")
	}
}

func TestUploadCancelledIsNotAnError(t *testing.T) {
	_, server := transferSetup(t)
	stubPicker(t, nil, desktop.ErrCancelled)
	if err := upload(false); err != nil {
		t.Fatalf("cancelled picker: %v", err)
	}
	if entries, _ := os.ReadDir(server); len(entries) != 0 {
		t.Fatalf("files appeared after a cancel: %v", entries)
	}
}

func TestUploadPickerFailureIsReported(t *testing.T) {
	transferSetup(t)
	stubPicker(t, nil, errors.New("zenity exploded"))
	err := upload(false)
	if err == nil || !strings.Contains(err.Error(), "could not show a file picker") {
		t.Fatalf("got %v", err)
	}
}
