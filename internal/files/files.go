package files

import (
	"archive/tar"
	"crypto/rand"
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

// maxEntries bounds how many files and directories one transfer may carry;
// lowered in tests.
var maxEntries = 100_000

// HumanSize formats a byte count for people: "512 B", "3.2 GiB", "2 GiB".
func HumanSize(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0") + " " + units[i]
}

// ValidName reports whether name can be the top-level name of a received file
// or directory: one path element of valid UTF-8 without control characters or
// separators. The desktop checks it before asking the user anything.
func ValidName(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return errors.New("unsafe file name")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return errors.New("unsafe file name")
		}
	}
	return nil
}

type entry struct {
	name string
	info os.FileInfo
}

// Prepare opens a regular file or produces a tar stream of a directory, for
// a transfer the desktop opens: executables and scripts are refused. The
// returned size is exact, including tar headers and padding. Close reader
// even when a transfer is interrupted, to stop the tar-producing goroutine.
func Prepare(p string) (name string, mode os.FileMode, size int64, reader io.ReadCloser, err error) {
	return prepare(p, true)
}

// PrepareDownload is Prepare for a file saved, never opened, on the other
// end: any regular file or directory goes, and a symlink given as p is
// followed (links inside a directory are still refused).
func PrepareDownload(p string) (name string, mode os.FileMode, size int64, reader io.ReadCloser, err error) {
	return prepare(p, false)
}

func prepare(p string, strict bool) (name string, mode os.FileMode, size int64, reader io.ReadCloser, err error) {
	stat := os.Lstat
	if !strict {
		stat = os.Stat
	}
	info, err := stat(p)
	if err != nil {
		return "", 0, 0, nil, err
	}
	name = safeName(filepath.Base(filepath.Clean(p)))
	if info.Mode().IsRegular() {
		if strict {
			if err = safeFile(info.Mode(), info.Name()); err != nil {
				return "", 0, 0, nil, err
			}
		}
		if info.Size() > MaxBytes {
			return "", 0, 0, nil, fmt.Errorf("file is %s, above the %s transfer limit", HumanSize(info.Size()), HumanSize(MaxBytes))
		}
		f, openErr := openChecked(p, info)
		if openErr != nil {
			return "", 0, 0, nil, openErr
		}
		if strict {
			if err = checkPrefix(f); err != nil {
				f.Close()
				return "", 0, 0, nil, err
			}
		}
		return name, info.Mode(), info.Size(), f, nil
	}
	if !info.IsDir() {
		return "", 0, 0, nil, fmt.Errorf("unsafe file type: %s", p)
	}
	if !strict {
		// The archive walks the directory itself, never through a link.
		if p, err = filepath.EvalSymlinks(p); err != nil {
			return "", 0, 0, nil, err
		}
	}
	entries, size, err := scanDirectory(p, info, strict)
	if err != nil {
		return "", 0, 0, nil, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeArchive(pw, p, info, entries, strict)) }()
	return name, info.Mode(), size, pr, nil
}

func scanDirectory(root string, original os.FileInfo, strict bool) ([]entry, int64, error) {
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
			if info.Size() > MaxBytes {
				return fmt.Errorf("%s is %s, above the %s transfer limit", p, HumanSize(info.Size()), HumanSize(MaxBytes))
			}
			if strict {
				if err = safeFile(info.Mode(), info.Name()); err != nil {
					return err
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
			return fmt.Errorf("directory archive exceeds the %s transfer limit", HumanSize(MaxBytes))
		}
		if len(entries) >= maxEntries {
			return fmt.Errorf("directory has more than %d entries, the transfer limit", maxEntries)
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

func writeArchive(dst io.Writer, path string, expected os.FileInfo, entries []entry, strict bool) error {
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
			if statErr == nil && strict {
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

// Receive stores exactly size bytes under a private directory within base,
// for the desktop to open: executables and scripts are refused. The
// returned path is the safe file or extracted directory; its parent must be
// removed by the caller when no longer needed.
func Receive(name string, mode os.FileMode, size int64, src io.Reader, base string) (result string, err error) {
	if err = checkIncoming(name, mode, size); err != nil {
		return "", err
	}
	if mode.IsRegular() {
		if err = safeFile(mode, name); err != nil {
			return "", err
		}
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
	if err = receiveInto(result, mode, size, src, true); err != nil {
		return "", err
	}
	return result, nil
}

// ReceiveDownload saves exactly size bytes as name in dir, never replacing
// anything there: a taken name becomes "name (1).ext", "name (2).ext"…
// Contents are not vetted (the file is saved, not opened), but nothing is
// made executable: files get mode 0666 and directories 0777, less the umask.
// Until complete, the transfer lives in a hidden .2o-partial-* directory.
func ReceiveDownload(name string, mode os.FileMode, size int64, src io.Reader, dir string) (string, error) {
	if err := checkIncoming(name, mode, size); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(dir, ".2o-partial-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, name)
	if err = receiveInto(staged, mode, size, src, false); err != nil {
		return "", err
	}
	return place(staged, dir, name, mode.IsDir())
}

func checkIncoming(name string, mode os.FileMode, size int64) error {
	if size < 0 || size > MaxBytes {
		return fmt.Errorf("invalid transfer size %d (limit %s)", size, HumanSize(MaxBytes))
	}
	if name != safeName(name) || name == "" || name == "." || name == ".." {
		return fmt.Errorf("unsafe name: %q", name)
	}
	switch {
	case mode.IsRegular():
	case mode.IsDir():
		if size < 1024 {
			return fmt.Errorf("invalid tar archive size: %d", size)
		}
	default:
		return fmt.Errorf("unsafe file mode: %v", mode)
	}
	return nil
}

// receiveInto writes the transfer at p, which must not exist.
func receiveInto(p string, mode os.FileMode, size int64, src io.Reader, strict bool) error {
	if mode.IsDir() {
		if err := os.Mkdir(p, perms(strict, true)); err != nil {
			return err
		}
		return extract(p, src, size, strict)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perms(strict, false))
	if err != nil {
		return err
	}
	if strict {
		err = copySafeFile(f, src, size)
	} else {
		_, err = io.CopyN(f, src, size)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// perms: private for the open cache, ordinary (umask) for downloads.
func perms(strict, dir bool) os.FileMode {
	switch {
	case strict && dir:
		return 0700
	case strict:
		return 0600
	case dir:
		return 0777
	}
	return 0666
}

// maxPlaceTries bounds the "name (N)" search.
const maxPlaceTries = 1000

// place moves staged into dir under name, or the first free "name (N)". A
// file is linked (link fails on any existing entry); a directory is renamed
// after checking the name is free (rename could only replace an empty
// directory created in between).
func place(staged, dir, name string, isDir bool) (string, error) {
	for i := range maxPlaceTries {
		target := filepath.Join(dir, numbered(name, i, isDir))
		var err error
		if isDir {
			if _, err = os.Lstat(target); err == nil {
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			err = os.Rename(staged, target)
		} else {
			err = os.Link(staged, target)
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return target, nil
	}
	return "", fmt.Errorf("%s and %d numbered copies already exist in %s", name, maxPlaceTries-1, dir)
}

// numbered is name for i = 0, else "base (i).ext" (directories have no extension).
func numbered(name string, i int, isDir bool) string {
	if i == 0 {
		return name
	}
	base, ext := name, ""
	if !isDir {
		ext = filepath.Ext(name)
		if ext == name { // ".bashrc"
			ext = ""
		}
		base = strings.TrimSuffix(name, ext)
	}
	return fmt.Sprintf("%s (%d)%s", base, i, ext)
}

// extract unpacks a tar stream of exactly size bytes. Besides confining paths,
// it never writes more file data than the announced size (sparse entries are
// refused, since they expand beyond their transported bytes) and stops after
// maxEntries entries. strict refuses executables and scripts.
func extract(dst string, src io.Reader, size int64, strict bool) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	limited := &io.LimitedReader{R: src, N: size}
	tr := tar.NewReader(limited)
	seen := make(map[string]struct{})
	budget := size // file bytes still allowed to be written
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(seen) >= maxEntries {
			return fmt.Errorf("archive has more than %d entries, the transfer limit", maxEntries)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := safeRelative(name); err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate archive path: %s", name)
		}
		seen[name] = struct{}{}
		if sparse(h) {
			return fmt.Errorf("sparse archive entry refused: %s", name)
		}
		// The reader already maps legacy '\x00' entries to TypeReg or TypeDir.
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, perms(strict, true)); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > budget {
				return fmt.Errorf("archive entry %s (%s) expands beyond the announced transfer size of %s", name, HumanSize(h.Size), HumanSize(size))
			}
			budget -= h.Size
			if strict {
				if err := safeFile(os.FileMode(h.Mode), path.Base(name)); err != nil {
					return err
				}
			}
			parent := path.Dir(name)
			if err := root.MkdirAll(parent, perms(strict, true)); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perms(strict, false))
			if err != nil {
				return err
			}
			if strict {
				err = copySafeFile(f, tr, h.Size)
			} else {
				_, err = io.CopyN(f, tr, h.Size)
			}
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

// sparse reports old GNU sparse entries and PAX (GNU.sparse.*) sparse
// entries, whose Size is the expanded size rather than the stored bytes.
func sparse(h *tar.Header) bool {
	if h.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
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
		return sum, 0, 0, fmt.Errorf("file is %s, above the %s transfer limit", HumanSize(info.Size()), HumanSize(MaxBytes))
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

// ErrConflict means the remote file changed (or appeared) while it was being
// edited on the desktop; nothing was written.
var ErrConflict = errors.New("edit conflict: file changed")

// Commit replaces path only if its current contents equal original. A staged
// file in the same directory makes the replacement atomic for readers.
func Commit(p string, original [32]byte, src io.Reader, size int64) error {
	f, err := stage(p, src, size, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	current, mode, _, err := Snapshot(p)
	if err != nil {
		f.Close()
		return err
	}
	if current != original {
		f.Close()
		return ErrConflict
	}
	if err = f.Chmod(mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)); err != nil {
		f.Close()
		return err
	}
	if err = syncClose(f); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// Create publishes a new file at p without ever replacing an existing one:
// if p appeared meanwhile, it returns ErrConflict. mode is filtered by the
// process umask, as for any newly created file.
func Create(p string, src io.Reader, size int64, mode os.FileMode) error {
	f, err := stage(p, src, size, mode.Perm())
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = syncClose(f); err != nil {
		return err
	}
	if err = os.Link(f.Name(), p); errors.Is(err, fs.ErrExist) {
		return ErrConflict
	}
	return err
}

// stage writes exactly size bytes from src to a new hidden file next to p.
// The caller removes it and must close it.
func stage(p string, src io.Reader, size int64, perm os.FileMode) (*os.File, error) {
	if size < 0 || size > MaxBytes {
		return nil, fmt.Errorf("invalid edit size %d (limit %s)", size, HumanSize(MaxBytes))
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	name := filepath.Join(filepath.Dir(p), fmt.Sprintf(".2o-edit-%x", suffix))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return nil, err
	}
	if _, err = io.CopyN(f, src, size); err != nil {
		f.Close()
		os.Remove(name)
		return nil, err
	}
	return f, nil
}

func syncClose(f *os.File) error {
	err := f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
