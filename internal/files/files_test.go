package files

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegularRoundTrip(t *testing.T) {
	remote := t.TempDir()
	local := t.TempDir()
	want := []byte("hello, world\n")
	p := filepath.Join(remote, "document.txt")
	if err := os.WriteFile(p, want, 0640); err != nil {
		t.Fatal(err)
	}
	name, mode, size, src, err := Prepare(p)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if name != "document.txt" || size != int64(len(want)) || mode.Perm() != 0640 {
		t.Fatalf("metadata: %q %v %d", name, mode, size)
	}
	got, err := Receive(name, mode, size, src, local)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("got %q, want %q", data, want)
	}
	stat, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("received permissions: %v", stat.Mode())
	}
	stat, err = os.Stat(filepath.Dir(got))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0700 {
		t.Fatalf("private directory permissions: %v", stat.Mode())
	}
}

func TestDirectoryRoundTrip(t *testing.T) {
	remote := t.TempDir()
	root := filepath.Join(remote, "project")
	if err := os.MkdirAll(filepath.Join(root, "deep", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deep", "note.txt"), []byte("a note"), 0644); err != nil {
		t.Fatal(err)
	}
	name, mode, size, src, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var archive bytes.Buffer
	copied, err := io.Copy(&archive, src)
	if err != nil {
		t.Fatal(err)
	}
	if size != copied {
		t.Fatalf("advertised tar size %d, produced %d", size, copied)
	}
	got, err := Receive(name, mode, size, bytes.NewReader(archive.Bytes()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(got, "deep", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a note" {
		t.Fatalf("file content %q", data)
	}
	stat, err := os.Stat(filepath.Join(got, "deep", "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v", stat.Mode())
	}
}

func TestArchiveExactSizeWithLongNamesAndEmptyDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archive")
	long := strings.Repeat("界", 60) + ".txt"
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, long), []byte("UTF-8 name"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{root, filepath.Join(t.TempDir(), "empty")} {
		if strings.HasSuffix(name, "empty") {
			if err := os.Mkdir(name, 0755); err != nil {
				t.Fatal(err)
			}
		}
		archiveName, mode, size, src, err := Prepare(name)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		n, copyErr := io.Copy(&b, src)
		closeErr := src.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if n != size {
			t.Fatalf("archive size advertised %d, got %d", size, n)
		}
		dst, err := Receive(archiveName, mode, size, bytes.NewReader(b.Bytes()), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if name == root {
			data, err := os.ReadFile(filepath.Join(dst, long))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "UTF-8 name" {
				t.Fatalf("round trip: %q", data)
			}
		}
	}
}

func TestDirectoryChangedAfterPrepareFailsTransfer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "document")
	if err := os.WriteFile(p, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, _, src, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, src); err == nil {
		t.Fatal("accepted replaced archive file")
	}
}

func TestRejectUnsafeFiles(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		mode           os.FileMode
	}{
		{"runme", "text", 0755},
		{"launcher.desktop", "[Desktop Entry]\nExec=anything\n", 0644},
		{"script.sh", "echo oops", 0644},
		{"plain", "#!/bin/sh\necho oops", 0644},
		{"binary", "\x7fELFfake", 0644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(p, []byte(tc.contents), tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, _, _, r, err := Prepare(p); err == nil {
				r.Close()
				t.Fatal("Prepare accepted unsafe file")
			}
			if _, err := Receive(tc.name, tc.mode, int64(len(tc.contents)), strings.NewReader(tc.contents), t.TempDir()); err == nil {
				t.Fatal("Receive accepted unsafe file")
			}
		})
	}
}

func TestRejectSymlinkInDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "folder")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := Prepare(root); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, _, _, _, err := Prepare(filepath.Join(root, "escape")); err == nil {
		t.Fatal("accepted symlink as root")
	}
}

func archive(t *testing.T, h *tar.Header, data string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRejectArchiveTraversalAndLinks(t *testing.T) {
	outside := t.TempDir()
	for _, tc := range []struct {
		name   string
		header tar.Header
		data   string
	}{
		{"parent", tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 3}, "bad"},
		{"absolute", tar.Header{Name: "/tmp/escape", Typeflag: tar.TypeReg, Size: 3}, "bad"},
		{"symlink", tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, ""},
		{"hardlink", tar.Header{Name: "escape", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}, ""},
		{"script", tar.Header{Name: "innocent", Typeflag: tar.TypeReg, Size: 6}, "#!/bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := archive(t, &tc.header, tc.data)
			_, err := Receive("folder", os.ModeDir|0755, int64(len(data)), bytes.NewReader(data), outside)
			if err == nil {
				t.Fatal("accepted malicious archive")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("left files after failed receive: %v", entries)
			}
		})
	}
}

func TestReceiveRejectsShortTransferAndUnsafeName(t *testing.T) {
	base := t.TempDir()
	if _, err := Receive("../escape", 0644, 4, strings.NewReader("data"), base); err == nil {
		t.Fatal("accepted unsafe name")
	}
	if _, err := Receive("safe.txt", 0644, 9, strings.NewReader("short"), base); err == nil {
		t.Fatal("accepted short transfer")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left partial transfer: %v", entries)
	}
}

func TestEditConflictAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(p, []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
	original, mode, size, err := Snapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if size != 6 || mode.Perm() != 0640 || original != sha256.Sum256([]byte("before")) {
		t.Fatalf("snapshot metadata: %v %d %x", mode, size, original)
	}
	if err := os.WriteFile(p, []byte("someone else's edit"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := Commit(p, original, strings.NewReader("mine"), 4); err == nil {
		t.Fatal("overwrote concurrent edit")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "someone else's edit" {
		t.Fatalf("lost concurrent change: %q", data)
	}
	fresh, _, _, err := Snapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(p, fresh, strings.NewReader("mine"), 4); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mine" {
		t.Fatalf("commit produced %q", data)
	}
	stat, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0640 {
		t.Fatalf("mode changed: %v", stat.Mode())
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", p); err != nil {
		t.Fatal(err)
	}
	if err := Commit(p, sha256.Sum256([]byte("/etc/passwd")), strings.NewReader("mine"), 4); err == nil {
		t.Fatal("committed through symlink")
	}
}

func TestEditExecutableScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho old\n"), 0750); err != nil {
		t.Fatal(err)
	}
	original, mode, _, err := Snapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Perm() != 0750 {
		t.Fatalf("snapshot mode: %v", mode)
	}
	replacement := "#!/bin/sh\necho new\n"
	if err := Commit(p, original, strings.NewReader(replacement), int64(len(replacement))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != replacement {
		t.Fatalf("script content: %q", data)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0750 {
		t.Fatalf("script permissions: %v", info.Mode())
	}
	if _, _, _, src, err := Prepare(p); err == nil {
		src.Close()
		t.Fatal("open accepted executable script")
	}
}
