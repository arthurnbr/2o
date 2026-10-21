package files

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxBytes is a protocol sanity ceiling (1 TiB), not a policy: the desktop
// daemon decides what it accepts (size cap, free space, confirmation).
const MaxBytes int64 = 1 << 40

type entry struct {
	name string
	info os.FileInfo
}

// Prepare opens a regular file or produces a tar stream of a directory. The
// returned size is exact, including tar headers and padding. Close reader even
// when a transfer is interrupted, to stop the tar-producing goroutine.
func Prepare(p string) (name string, mode os.FileMode, size int64, reader io.ReadCloser, err error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", 0, 0, nil, err
	}
	name = safeName(filepath.Base(filepath.Clean(p)))
	if info.Mode().IsRegular() {
		if err = safeFile(info.Mode(), info.Name()); err != nil {
			return "", 0, 0, nil, err
		}
		if info.Size() > MaxBytes {
			return "", 0, 0, nil, fmt.Errorf("file exceeds %d bytes", MaxBytes)
		}
		f, openErr := openChecked(p, info)
		if openErr != nil {
			return "", 0, 0, nil, openErr
		}
		if err = checkPrefix(f); err != nil {
			f.Close()
			return "", 0, 0, nil, err
		}
		return name, info.Mode(), info.Size(), f, nil
	}
	if !info.IsDir() {
		return "", 0, 0, nil, fmt.Errorf("unsafe file type: %s", p)
	}
	entries, size, err := scanDirectory(p, info)
	if err != nil {
		return "", 0, 0, nil, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeArchive(pw, p, info, entries)) }()
	return name, info.Mode(), size, pr, nil
}

func scanDirectory(root string, original os.FileInfo) ([]entry, int64, error) {
	var entries []entry
	var count countingWriter
	tw := tar.NewWriter(&count)
	var payload int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			current, err := d.Info()
			if err != nil {
				return err
			}
			if !current.IsDir() || !os.SameFile(original, current) {
				return fmt.Errorf("directory changed during transfer: %s", root)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe file type: %s", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err = safeRelative(rel); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err = safeFile(info.Mode(), info.Name()); err != nil {
				return err
			}
			if info.Size() > MaxBytes {
				return fmt.Errorf("file exceeds %d bytes: %s", MaxBytes, p)
			}
			f, err := openChecked(p, info)
			if err != nil {
				return err
			}
			err = checkPrefix(f)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		h := archiveHeader(rel, info.Mode(), info.Size())
		// A zero-sized header has identical metadata overhead; file data is
		// accounted for arithmetically rather than read twice.
		h.Size = 0
		if err = tw.WriteHeader(&h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			payload += (info.Size() + 511) / 512 * 512
		}
		if count.n+payload+1024 > MaxBytes {
			return fmt.Errorf("directory exceeds %d archive bytes", MaxBytes)
		}
		entries = append(entries, entry{rel, info})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err = tw.Close(); err != nil {
		return nil, 0, err
	}
	return entries, count.n + payload, nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

func archiveHeader(name string, mode os.FileMode, size int64) tar.Header {
	h := tar.Header{Name: name, Mode: int64(mode.Perm())}
	if mode.IsDir() {
		h.Typeflag = tar.TypeDir
		h.Name += "/"
	} else {
		h.Typeflag = tar.TypeReg
		h.Size = size
	}
	return h
}

func writeArchive(dst io.Writer, path string, expected os.FileInfo, entries []entry) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	current, err := root.Lstat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("directory changed during transfer: %s", path)
	}
	tw := tar.NewWriter(dst)
	for _, e := range entries {
		info, err := root.Lstat(e.name)
		if err != nil {
			return err
		}
		if !os.SameFile(e.info, info) || info.Mode().Type() != e.info.Mode().Type() || info.Size() != e.info.Size() {
			return fmt.Errorf("file changed during transfer: %s", e.name)
		}
		h := archiveHeader(e.name, e.info.Mode(), e.info.Size())
		if err = tw.WriteHeader(&h); err != nil {
			return err
		}
		if e.info.Mode().IsRegular() {
			f, err := root.Open(e.name)
			if err != nil {
				return err
			}
			opened, statErr := f.Stat()
			if statErr == nil && (!opened.Mode().IsRegular() || !os.SameFile(e.info, opened) || opened.Size() != e.info.Size()) {
				statErr = fmt.Errorf("file changed during transfer: %s", e.name)
			}
			if statErr == nil {
				statErr = checkPrefix(f)
			}
			if statErr == nil {
				_, statErr = io.CopyN(tw, f, e.info.Size())
			}
			closeErr := f.Close()
			if statErr != nil {
				return statErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return tw.Close()
}

// Receive stores exactly size bytes under a private directory within base.
// The returned path is the safe file or extracted directory; its parent must
// be removed by the caller when no longer needed.
func Receive(name string, mode os.FileMode, size int64, src io.Reader, base string) (result string, err error) {
	if size < 0 || size > MaxBytes {
		return "", fmt.Errorf("invalid transfer size: %d", size)
	}
	if name != safeName(name) || name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("unsafe name: %q", name)
	}
	if mode.IsRegular() {
		if err = safeFile(mode, name); err != nil {
			return "", err
		}
	} else if mode.IsDir() {
		if size < 1024 {
			return "", fmt.Errorf("invalid tar archive size: %d", size)
		}
	} else {
		return "", fmt.Errorf("unsafe file mode: %v", mode)
	}
	dir, err := os.MkdirTemp(base, "2o-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	result = filepath.Join(dir, name)
	if mode.IsDir() {
		if err = os.Mkdir(result, 0700); err != nil {
			return "", err
		}
		err = extract(result, src, size)
	} else {
		var f *os.File
		f, err = os.OpenFile(result, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			err = copySafeFile(f, src, size)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return "", err
	}
	return result, nil
}

func extract(dst string, src io.Reader, size int64) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	limited := &io.LimitedReader{R: src, N: size}
	tr := tar.NewReader(limited)
	seen := make(map[string]struct{})
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := safeRelative(name); err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate archive path: %s", name)
		}
		seen[name] = struct{}{}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > MaxBytes {
				return fmt.Errorf("invalid archive file size: %d", h.Size)
			}
			if err := safeFile(os.FileMode(h.Mode), path.Base(name)); err != nil {
				return err
			}
			parent := path.Dir(name)
			if err := root.MkdirAll(parent, 0700); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			err = copySafeFile(f, tr, h.Size)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsafe archive entry type: %q", h.Typeflag)
		}
	}
	_, err = io.CopyN(io.Discard, limited, limited.N)
	return err
}

func copySafeFile(dst io.Writer, src io.Reader, size int64) error {
	var prefix [4]byte
	n := min(size, int64(len(prefix)))
	if _, err := io.ReadFull(src, prefix[:n]); err != nil {
		return err
	}
	if unsafePrefix(prefix[:n]) {
		return errors.New("executable content refused")
	}
	if _, err := dst.Write(prefix[:n]); err != nil {
		return err
	}
	_, err := io.CopyN(dst, src, size-n)
	return err
}

func checkPrefix(f *os.File) error {
	var prefix [4]byte
	n, err := f.Read(prefix[:])
	if err != nil && err != io.EOF {
		return err
	}
	if unsafePrefix(prefix[:n]) {
		return errors.New("executable content refused")
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

func unsafePrefix(p []byte) bool {
	return (len(p) >= 2 && (string(p[:2]) == "#!" || string(p[:2]) == "MZ")) || (len(p) >= 4 && (string(p[:4]) == "\x7fELF" || string(p[:4]) == "\xfe\xed\xfa\xce" || string(p[:4]) == "\xcf\xfa\xed\xfe" || string(p[:4]) == "\xca\xfe\xba\xbe"))
}

func safeFile(mode os.FileMode, name string) error {
	if !mode.IsRegular() || mode.Perm()&0111 != 0 {
		return fmt.Errorf("executable or unsafe file refused: %s", name)
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".desktop", ".sh", ".bash", ".zsh", ".fish", ".ksh", ".csh", ".command", ".bat", ".cmd", ".ps1", ".appimage", ".run":
		return fmt.Errorf("executable or unsafe file refused: %s", name)
	}
	return nil
}

func safeName(name string) string {
	if name == "" || name == "." || name == ".." || name == "/" {
		return "directory"
	}
	if !utf8.ValidString(name) {
		return "file"
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, name)
}

func safeRelative(name string) error {
	if name == "" || name != path.Clean(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("unsafe archive path: %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part != safeName(part) {
			return fmt.Errorf("unsafe archive path: %q", name)
		}
	}
	return nil
}

func openChecked(p string, before os.FileInfo) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && (!after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != before.Size()) {
		err = fmt.Errorf("file changed during transfer: %s", p)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Snapshot hashes a regular file for an optimistic edit, including scripts
// which are editable but must never be opened via the open transfer.
func Snapshot(p string) (sum [32]byte, mode os.FileMode, size int64, err error) {
	info, err := os.Lstat(p)
	if err != nil {
		return sum, 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return sum, 0, 0, fmt.Errorf("not a regular file: %s", p)
	}
	if info.Size() > MaxBytes {
		return sum, 0, 0, fmt.Errorf("file exceeds %d bytes", MaxBytes)
	}
	f, err := openChecked(p, info)
	if err != nil {
		return sum, 0, 0, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.CopyN(h, f, info.Size()); err != nil {
		return sum, 0, 0, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, info.Mode(), info.Size(), nil
}

// Commit replaces path only if its current contents equal original. A staged
// file in the same directory makes the replacement atomic for readers.
func Commit(p string, original [32]byte, src io.Reader, size int64) error {
	if size < 0 || size > MaxBytes {
		return fmt.Errorf("invalid edit size: %d", size)
	}
	parent := filepath.Dir(p)
	f, err := os.CreateTemp(parent, ".2o-edit-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = io.CopyN(f, src, size); err != nil {
		f.Close()
		return err
	}
	current, mode, _, err := Snapshot(p)
	if err != nil {
		f.Close()
		return err
	}
	if current != original {
		f.Close()
		return errors.New("edit conflict: file changed")
	}
	if err = f.Chmod(mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
