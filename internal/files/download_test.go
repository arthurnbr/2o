package files

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNumbered(t *testing.T) {
	for _, tc := range []struct {
		name string
		i    int
		dir  bool
		want string
	}{
		{"report.pdf", 0, false, "report.pdf"},
		{"report.pdf", 2, false, "report (2).pdf"},
		{"archive.tar.gz", 1, false, "archive.tar (1).gz"},
		{".bashrc", 1, false, ".bashrc (1)"},
		{"noext", 3, false, "noext (3)"},
		{"v1.2", 1, true, "v1.2 (1)"},
	} {
		if got := numbered(tc.name, tc.i, tc.dir); got != tc.want {
			t.Errorf("numbered(%q, %d, %v) = %q, want %q", tc.name, tc.i, tc.dir, got, tc.want)
		}
	}
}

// A download through a symlink sends the target under the link's name; the
// strict open transfer refuses the link.
func TestPrepareDownloadFollowsTopLevelLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	_ = os.WriteFile(target, []byte("x"), 0644)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := Prepare(link); err == nil {
		t.Fatal("open transfer accepted a symlink")
	}
	name, _, size, r, err := PrepareDownload(link)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if name != "link.txt" || size != 1 {
		t.Fatalf("got %q %d", name, size)
	}
}

func TestReceiveDownloadTruncatedLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReceiveDownload("f.bin", 0644, 10, bytes.NewReader([]byte("short")), dir)
	if err == nil {
		t.Fatal("truncated transfer accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestReceiveDownloadRefusesUnsafeNames(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if _, err := ReceiveDownload(n, 0644, 1, bytes.NewReader([]byte("x")), t.TempDir()); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}
