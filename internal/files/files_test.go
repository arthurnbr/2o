package files

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
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

// block builds one raw 512-byte tar header, so tests can produce entries the
// standard writer refuses to emit (old GNU sparse, legacy '\x00' files).
func block(name string, typeflag byte, size int64, gnu bool, patch func(b []byte)) []byte {
	b := make([]byte, 512)
	copy(b[0:], name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	b[156] = typeflag
	if gnu {
		copy(b[257:], "ustar  \x00")
	} else {
		copy(b[257:], "ustar\x0000")
	}
	if patch != nil {
		patch(b)
	}
	copy(b[148:], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func pad(data []byte) []byte {
	return append(data, make([]byte, (512-len(data)%512)%512)...)
}

func paxRecords(kv ...string) []byte {
	var b bytes.Buffer
	for i := 0; i < len(kv); i += 2 {
		rec := " " + kv[i] + "=" + kv[i+1] + "\n"
		n := len(rec) + 1
		for len(fmt.Sprint(n))+len(rec) != n {
			n++
		}
		fmt.Fprintf(&b, "%d%s", n, rec)
	}
	return b.Bytes()
}

func receiveRaw(t *testing.T, parts ...[]byte) (dst, base string, err error) {
	t.Helper()
	data := bytes.Join(append(parts, make([]byte, 1024)), nil)
	base = t.TempDir()
	dst, err = Receive("folder", os.ModeDir|0755, int64(len(data)), bytes.NewReader(data), base)
	return dst, base, err
}

// A sparse entry transports a few bytes but expands to its "real" size: the
// review measured 10 KiB announced, 64 MiB written. Both encodings are refused
// and nothing is left behind.
func TestRejectSparseArchiveEntries(t *testing.T) {
	const real = 64 << 20
	payload := pad([]byte("tail"))
	oldGNU := block("bomb", 'S', int64(len(payload)), true, func(b []byte) {
		copy(b[386:], fmt.Sprintf("%011o\x00", real-512)) // sparse map: data at the end
		copy(b[398:], fmt.Sprintf("%011o\x00", 512))
		copy(b[483:], fmt.Sprintf("%011o\x00", real)) // realsize
	})
	records := paxRecords("GNU.sparse.major", "1", "GNU.sparse.minor", "0", "GNU.sparse.name", "bomb", "GNU.sparse.realsize", fmt.Sprint(real))
	sparseMap := pad([]byte("1\n" + fmt.Sprint(real-512) + "\n512\n"))
	paxData := append(sparseMap, payload...)
	for name, parts := range map[string][][]byte{
		"old GNU": {oldGNU, payload},
		"PAX 1.0": {block("pax", 'x', int64(len(records)), false, nil), pad(records), block("GNUSparseFile.0/bomb", '0', int64(len(paxData)), false, nil), paxData},
		"PAX unknown version": {
			block("pax", 'x', int64(len(paxRecords("GNU.sparse.major", "9"))), false, nil), pad(paxRecords("GNU.sparse.major", "9")),
			block("bomb", '0', int64(len(payload)), false, nil), payload,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, base, err := receiveRaw(t, parts...)
			if err == nil {
				t.Fatal("accepted sparse archive")
			}
			if !strings.Contains(err.Error(), "sparse") {
				t.Fatalf("refused for another reason: %v", err)
			}
			if entries, _ := os.ReadDir(base); len(entries) != 0 {
				t.Fatalf("left files after refused sparse archive: %v", entries)
			}
		})
	}
}

// Old archives mark regular files with '\x00' instead of '0'.
func TestLegacyRegularEntryAccepted(t *testing.T) {
	data := pad([]byte("hello"))
	dst, _, err := receiveRaw(t, block("old.txt", 0, 5, false, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "old.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("legacy entry: %q %v", got, err)
	}
}

func TestEntryCount(t *testing.T) {
	old := maxEntries
	maxEntries = 3
	t.Cleanup(func() { maxEntries = old })

	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for i := range 4 {
		if err := w.WriteHeader(&tar.Header{Name: fmt.Sprintf("d%d/", i), Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	_, err := Receive("folder", os.ModeDir|0755, int64(b.Len()), bytes.NewReader(b.Bytes()), base)
	if err == nil || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Fatalf("entry cap not enforced on receive: %v", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("left files: %v", entries)
	}

	root := filepath.Join(t.TempDir(), "many")
	for i := range 3 {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprint(i)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, src, err := Prepare(root); err != nil {
		t.Fatalf("3 entries is within the cap: %v", err)
	} else {
		src.Close()
	}
	if err := os.Mkdir(filepath.Join(root, "3"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := Prepare(root); err == nil {
		t.Fatal("entry cap not enforced on send")
	}
}

func TestCreateNeverReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.txt")
	if err := Create(p, strings.NewReader("first"), 5, 0640); err != nil {
		t.Fatal(err)
	}
	if err := Create(p, strings.NewReader("second"), 6, 0640); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing file: want ErrConflict, got %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "first" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("staging file left behind: %v", entries)
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1 << 10: "1 KiB", 3<<30 + 1<<28: "3.2 GiB", 2 << 30: "2 GiB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
