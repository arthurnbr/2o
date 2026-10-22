package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/ui"
)

// Desktop-side transfer policy. Everything here is decided by the machine
// that receives data; the remote side can only announce sizes.
const (
	defaultMaxBytes     int64 = 0       // 0 = no fixed cap; free disk space still applies
	defaultConfirmBytes int64 = 1 << 30 // ask before receiving more than 1 GiB
	defaultClipBytes    int64 = 16 << 20
	defaultCacheDays          = 7
	diskReserve         int64 = 1 << 30 // always leave 1 GiB free
	maxDisplayName            = 60      // runes of a remote name shown to the user
)

// sizeEnv parses sizes such as "512M", "2G", "100k", "0". Bad values fall
// back to def so a typo never silently disables protection.
func sizeEnv(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := parseSize(v)
	if err != nil {
		ui.Warn("ignoring %s=%q (%v), using %s", key, v, err, humanSize(def))
		return def
	}
	return n
}

func parseSize(v string) (int64, error) {
	mult := int64(1)
	switch strings.ToUpper(v[len(v)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size")
	}
	return n * mult, nil
}

func humanSize(n int64) string { return files.HumanSize(n) }

// displayName makes a remote-provided name safe to show in notifications and
// logs: control and format characters (including bidi overrides) are dropped,
// whitespace runs become one space, and long names are shortened.
func displayName(name string) string {
	var b strings.Builder
	space, n := false, 0
	for _, r := range strings.ToValidUTF8(name, "�") {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if n == maxDisplayName {
			b.WriteString("…")
			break
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return "unnamed file"
	}
	return b.String()
}

// confirm asks the desktop user; replaced in tests.
var confirm = desktop.Confirm

// statfs reports the bytes available to unprivileged users under dir;
// replaced in tests so decisions do not depend on the real disk.
var statfs = func(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// inFlight is the number of bytes admitted but not yet released. Free space
// already shrinks as those transfers write, so subtracting their full size
// again is conservative: concurrent transfers can never overcommit the disk.
var inFlight struct {
	sync.Mutex
	bytes int64
}

func refused(format string, a ...any) *userError { return fail(format, a...).exit(exitRefused) }

// admitTransfer decides, before any payload is read, whether the desktop
// accepts size bytes named name into dir. On success the bytes stay reserved
// against TWO_O_MAX_BYTES and free space until release is called, which the
// caller must do when the transfer ends, whatever its outcome. onWait (may be
// nil) runs right before the user is asked to confirm.
func admitTransfer(name string, size int64, dir string, onWait func()) (release func(), err error) {
	if err := files.ValidName(name); err != nil {
		return nil, refused("refusing a file with an unsafe name (%q)", displayName(name)).because(err)
	}
	shown := displayName(name)
	if size < 0 || size > files.MaxBytes {
		return nil, refused("%s announces an invalid size (%d bytes)", shown, size)
	}
	limit := sizeEnv("TWO_O_MAX_BYTES", defaultMaxBytes)
	if limit > 0 && size > limit {
		return nil, refused("%s is %s, above this desktop's limit of %s (TWO_O_MAX_BYTES)", shown, humanSize(size), humanSize(limit)).
			hint("raise TWO_O_MAX_BYTES on the desktop, or send a smaller file")
	}
	if release, err = reserve(shown, size, dir, limit); err != nil {
		return nil, err
	}
	threshold := sizeEnv("TWO_O_CONFIRM_BYTES", defaultConfirmBytes)
	if threshold <= 0 || size <= threshold {
		return release, nil
	}
	if onWait != nil {
		onWait()
	}
	ok, err := confirm("2o: large transfer", fmt.Sprintf("Receive %s (%s) from the SSH server?", shown, humanSize(size)))
	switch {
	case errors.Is(err, desktop.ErrTimeout):
		release()
		return nil, refused("nobody answered the desktop prompt for %s (%s) in time", shown, humanSize(size)).because(err).
			hint("accept the notification on the desktop, or raise TWO_O_CONFIRM_BYTES (now %s) there", humanSize(threshold))
	case err != nil:
		release()
		ui.Warn("cannot ask to confirm %s (%s): %v", shown, humanSize(size), err)
		return nil, refused("%s is %s, above this desktop's confirmation threshold of %s (TWO_O_CONFIRM_BYTES), and no prompt could be shown there", shown, humanSize(size), humanSize(threshold)).because(err).
			hint("run a notification daemon on the desktop, or raise TWO_O_CONFIRM_BYTES there")
	case !ok:
		release()
		return nil, refused("%s (%s) was declined on the desktop", shown, humanSize(size))
	}
	return release, nil
}

// reserve atomically checks size against the in-flight cap and free space and
// books it. The returned release is idempotent.
func reserve(shown string, size int64, dir string, limit int64) (func(), error) {
	inFlight.Lock()
	defer inFlight.Unlock()
	if limit > 0 && inFlight.bytes+size > limit {
		return nil, refused("%s is %s, but %s is already being received and this desktop's limit is %s (TWO_O_MAX_BYTES)", shown, humanSize(size), humanSize(inFlight.bytes), humanSize(limit)).
			hint("wait for the other transfers to finish, or raise TWO_O_MAX_BYTES on the desktop")
	}
	if free, err := statfs(dir); err == nil {
		if avail := free - diskReserve - inFlight.bytes; size > avail {
			msg := fmt.Sprintf("%s is %s, but the desktop only has %s free (keeping %s spare)", shown, humanSize(size), humanSize(max(free, 0)), humanSize(diskReserve))
			if inFlight.bytes > 0 {
				msg += fmt.Sprintf(" and %s is already being received", humanSize(inFlight.bytes))
			}
			return nil, refused("%s", msg).hint("free space on the desktop, then retry")
		}
	}
	inFlight.bytes += size
	var once sync.Once
	return func() {
		once.Do(func() {
			inFlight.Lock()
			inFlight.bytes -= size
			inFlight.Unlock()
		})
	}, nil
}

func clipLimit() int64 { return sizeEnv("TWO_O_CLIPBOARD_BYTES", defaultClipBytes) }

// cleanCache removes received copies older than TWO_O_CACHE_DAYS (0 keeps
// everything). Edit recovery copies are kept: they may be unsaved work.
func cleanCache(dir string, now time.Time) {
	days := defaultCacheDays
	if v := strings.TrimSpace(os.Getenv("TWO_O_CACHE_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			ui.Warn("ignoring TWO_O_CACHE_DAYS=%q, keeping copies %d days", v, defaultCacheDays)
		} else {
			days = n
		}
	}
	if days == 0 {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "2o-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// cacheJanitor cleans at start and then every few hours while the daemon runs.
func cacheJanitor() {
	for {
		cleanCache(cacheDir(), time.Now())
		time.Sleep(6 * time.Hour)
	}
}
