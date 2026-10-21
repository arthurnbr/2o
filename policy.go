package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/desktop"
)

// Desktop-side transfer policy. Everything here is decided by the machine
// that receives data; the remote side can only announce sizes.
const (
	defaultMaxBytes     int64 = 0       // 0 = no fixed cap; free disk space still applies
	defaultConfirmBytes int64 = 1 << 30 // ask before receiving more than 1 GiB
	defaultClipBytes    int64 = 16 << 20
	defaultCacheDays          = 7
	diskReserve         int64 = 1 << 30 // always leave 1 GiB free
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
		fmt.Fprintf(os.Stderr, "2o: ignoring %s=%q: %v\n", key, v, err)
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

func humanSize(n int64) string {
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
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// confirm asks the desktop user; replaced in tests.
var confirm = desktop.Confirm

// admitTransfer decides, before any payload is read, whether the desktop
// accepts size bytes named name into dir.
func admitTransfer(name string, size int64, dir string) error {
	if limit := sizeEnv("TWO_O_MAX_BYTES", defaultMaxBytes); limit > 0 && size > limit {
		return fmt.Errorf("%s is %s, above this desktop's TWO_O_MAX_BYTES (%s)", name, humanSize(size), humanSize(limit))
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		if size > free-diskReserve {
			return fmt.Errorf("%s is %s but only %s is free on the desktop (keeping %s spare)", name, humanSize(size), humanSize(max(free, 0)), humanSize(diskReserve))
		}
	}
	if threshold := sizeEnv("TWO_O_CONFIRM_BYTES", defaultConfirmBytes); threshold > 0 && size > threshold {
		ok, err := confirm("2o: large transfer", fmt.Sprintf("Receive %s (%s) from the SSH server?", name, humanSize(size)))
		if err != nil {
			return fmt.Errorf("transfer of %s needs confirmation, which is unavailable: %w", humanSize(size), err)
		}
		if !ok {
			return fmt.Errorf("transfer of %s (%s) refused on the desktop", name, humanSize(size))
		}
	}
	return nil
}

func clipLimit() int64 { return sizeEnv("TWO_O_CLIPBOARD_BYTES", defaultClipBytes) }

// cleanCache removes received copies older than TWO_O_CACHE_DAYS (0 keeps
// everything). Edit recovery copies are kept: they may be unsaved work.
func cleanCache(dir string, now time.Time) {
	days := defaultCacheDays
	if v := strings.TrimSpace(os.Getenv("TWO_O_CACHE_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "2o: ignoring TWO_O_CACHE_DAYS=%q\n", v)
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
