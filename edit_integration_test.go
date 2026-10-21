package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditConflictKeepsLocalRecoveryCopy(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	remote := filepath.Join(dir, "original.txt")
	if err := os.WriteFile(remote, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(dir, "editor")
	script := "#!/bin/sh\nprintf 'someone else changed it' > \"$TWO_O_TEST_REMOTE\"\nprintf 'my edited copy' > \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_EDITOR", editor)
	t.Setenv("TWO_O_TEST_REMOTE", remote)
	t.Setenv("SSH_CONNECTION", "loopback")
	sock := filepath.Join(dir, "2o.sock")
	t.Setenv("TWO_O_SOCK", sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := l.Accept()
		if e == nil {
			defer c.Close()
			serve(c)
		}
	}()
	err = editFile(remote)
	if err == nil || !strings.Contains(err.Error(), "edit conflict") || !strings.Contains(err.Error(), "recovery copy:") {
		t.Fatalf("expected conflict with recovery path, got %v", err)
	}
	path := strings.TrimPrefix(err.Error()[strings.Index(err.Error(), "recovery copy:"):], "recovery copy: ")
	if !filepath.IsAbs(path) {
		t.Fatalf("recovery path not absolute: %q", path)
	}
	copy, copyErr := os.ReadFile(path)
	if copyErr != nil || string(copy) != "my edited copy" {
		t.Fatalf("recovery content %q: %v", copy, copyErr)
	}
	current, readErr := os.ReadFile(remote)
	if readErr != nil || string(current) != "someone else changed it" {
		t.Fatalf("remote file %q: %v", current, readErr)
	}
	<-done
}
