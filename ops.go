package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/wire"
)

// replyErr sends a failure to the remote. A *userError keeps its hints and
// exit code; any other error becomes a plain message with exit code 1, so the
// remote always gets a human-readable *userError back (see replyError).
func replyErr(c net.Conn, err error) {
	m := wire.Message{Error: err.Error(), Code: 1, Desktop: desktopLabel()}
	var ue *userError
	if errors.As(err, &ue) {
		m.Error, m.Hints = ue.msg, ue.hints
		if ue.code != 0 {
			m.Code = ue.code
		}
	}
	_ = wire.Write(c, m)
}

// ok acknowledges a request that succeeded.
func ok(c net.Conn) { _ = wire.Write(c, wire.Message{Desktop: desktopLabel()}) }

func serve(c net.Conn, m wire.Message) {
	switch m.Op {
	case "ping":
		_ = wire.Write(c, wire.Message{Desktop: desktopLabel(), Version: version})
	case "doctor":
		_ = wire.Write(c, wire.Message{Desktop: desktopLabel(), Version: version, Checks: doctorChecks()})
	case "open-url":
		serveOpenURL(c, m)
	case "open-file":
		serveOpenFile(c, m)
	case "copy":
		serveCopy(c, m)
	case "paste":
		servePaste(c)
	case "notify":
		serveNotify(c, m)
	case "edit":
		serveEdit(c, m)
	case "proxy":
		serveProxy(c, m)
	case "proxy-stream":
		serveProxyStream(c, m)
	default:
		replyErr(c, unknownOp(m.Op))
	}
}

func serveCopy(c net.Conn, m wire.Message) {
	if limit := clipLimit(); m.Size < 0 || m.Size > limit {
		replyErr(c, fail("clipboard content is %s; this desktop accepts up to %s", humanSize(m.Size), humanSize(limit)).
			hint("raise TWO_O_CLIPBOARD_BYTES on the desktop, or use 2o open FILE").exit(exitRefused))
		return
	}
	b, err := io.ReadAll(io.LimitReader(c, m.Size))
	if err == nil && int64(len(b)) != m.Size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		replyErr(c, fail("clipboard content was cut off in transit").because(err))
		return
	}
	if err = desktop.Copy(b); err != nil {
		replyErr(c, desktopFailure("could not set the desktop clipboard", err))
		return
	}
	ok(c)
}

func servePaste(c net.Conn) {
	limit := clipLimit()
	b, err := desktop.PasteLimit(limit)
	if errors.Is(err, desktop.ErrTooLarge) {
		replyErr(c, fail("the desktop clipboard is larger than %s", humanSize(limit)).
			hint("raise TWO_O_CLIPBOARD_BYTES on the desktop").because(err).exit(exitRefused))
		return
	}
	if err != nil {
		replyErr(c, desktopFailure("could not read the desktop clipboard", err))
		return
	}
	if err = wire.Write(c, wire.Message{Size: int64(len(b)), Desktop: desktopLabel()}); err == nil {
		_, _ = c.Write(b)
	}
}

func serveNotify(c net.Conn, m wire.Message) {
	if len(m.Title) > 512 || len(m.Body) > 4096 {
		replyErr(c, fail("notification is too long: title %d bytes (limit 512), body %d bytes (limit 4096)", len(m.Title), len(m.Body)).exit(exitUsage))
		return
	}
	if err := desktop.Notify(m.Title, m.Body); err != nil {
		replyErr(c, desktopFailure("could not show the notification", err))
		return
	}
	ok(c)
}

// desktopFailure explains a desktop tool failure: the tool's own message is
// the detail, and a missing integration points at `2o doctor`.
func desktopFailure(what string, err error) *userError {
	e := fail("%s: %v", what, err).because(err)
	if errors.Is(err, desktop.ErrUnavailable) {
		e.hint("run '2o doctor' to see what the desktop supports")
	}
	return e
}

func unknownOp(op string) error {
	return fail("the desktop does not know the request %q", op).
		hint("update 2o on the desktop to %s", version).exit(exitUsage)
}

func serveOpenURL(c net.Conn, m wire.Message) {
	if !httpURL(m.URL) {
		replyErr(c, fail("only http and https URLs can be opened").exit(exitUsage))
		return
	}
	if err := desktop.Open(m.URL); err != nil {
		replyErr(c, openError(m.URL, err))
		return
	}
	ok(c)
}

func serveOpenFile(c net.Conn, m wire.Message) {
	base := cacheDir()
	if err := os.MkdirAll(base, 0700); err != nil {
		replyErr(c, err)
		return
	}
	release, err := admitTransfer(m.Name, m.Size, base, waiting(c))
	if err != nil {
		replyErr(c, err)
		return
	}
	defer release()
	// The remote waits for this go-ahead before streaming any payload.
	if err = wire.Write(c, wire.Message{Op: "accept", Desktop: desktopLabel()}); err != nil {
		return
	}
	path, err := files.Receive(m.Name, os.FileMode(m.Mode), m.Size, c, base)
	if err != nil {
		replyErr(c, fail("the desktop could not receive %s", displayName(m.Name)).because(err).hint("%v", err))
		return
	}
	if err = desktop.Open(path); err != nil {
		replyErr(c, openError(path, err).hint("the copy is in %s", path))
		return
	}
	ok(c)
}

// waiting returns admitTransfer's hook telling the remote that the desktop
// user is being asked to confirm, so it can say so instead of hanging.
func waiting(c net.Conn) func() {
	return func() { _ = wire.Write(c, wire.Message{Op: "waiting", Desktop: desktopLabel()}) }
}

func openError(target string, err error) *userError {
	return fail("the desktop could not open %s", displayName(filepath.Base(target))).because(err).
		hint("%v", err).hint("run 2o doctor to check the desktop")
}

// doctorChecks is the desktop half of `2o doctor`: integrations plus what
// only the daemon knows (its log, its cache, the effective policy).
func doctorChecks() []wire.Check {
	var out []wire.Check
	for _, r := range desktop.Doctor() {
		out = append(out, wire.Check{Name: r.Name, OK: r.OK, Detail: r.Detail})
	}
	limit := "none"
	if n := sizeEnv("TWO_O_MAX_BYTES", defaultMaxBytes); n > 0 {
		limit = humanSize(n)
	}
	confirmAt := "never"
	if n := sizeEnv("TWO_O_CONFIRM_BYTES", defaultConfirmBytes); n > 0 {
		confirmAt = "above " + humanSize(n)
	}
	out = append(out, wire.Check{Name: "transfer policy", OK: true,
		Detail: fmt.Sprintf("max %s, confirm %s, clipboard %s", limit, confirmAt, humanSize(clipLimit()))})
	log := filepath.Join(cacheDir(), "daemon.log")
	if _, err := os.Stat(log); err == nil {
		out = append(out, wire.Check{Name: "daemon log", OK: true, Detail: log})
	} else {
		out = append(out, wire.Check{Name: "daemon log", OK: true, Detail: "none (daemon not started by 2o ssh)"})
	}
	return out
}

// serveEdit runs one edit on the desktop. The edited copy lives in a private
// edit-* directory of the cache (never cleaned automatically). It is deleted
// only when the remote confirms it wrote the result, or when the editor left
// it unchanged; in every other case (editor failure, lost connection,
// conflict) it is kept and its path reported, so saved work is never lost.
//
// Protocol: remote → {Op: edit, Name, Size, Mode, Line} + Size bytes;
// desktop → {Op: waiting}? then {Op: accept, Path} (the recovery path, known
// before the editor starts); after the editor: {Op: unchanged}, or
// {Size, Path} + Size bytes then remote → ack {} or {Error}, and after an
// ack the desktop removes the copy and answers {}. Failures are replyErr
// messages whose hints carry the recovery path.
func serveEdit(c net.Conn, m wire.Message) {
	name := displayName(m.Name)
	cache := cacheDir()
	if e := os.MkdirAll(cache, 0700); e != nil {
		replyErr(c, e)
		return
	}
	release, e := admitTransfer(m.Name, m.Size, cache, waiting(c))
	if e != nil {
		replyErr(c, e)
		return
	}
	defer release()
	dir, e := os.MkdirTemp(cache, "edit-")
	if e != nil {
		replyErr(c, e)
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, m.Name)
	saved := func(format string, a ...any) *userError {
		return fail(format, a...).hint("your edits are safe in %s on %s", path, desktopLabel())
	}
	if e = wire.Write(c, wire.Message{Op: "accept", Path: path, Desktop: desktopLabel()}); e != nil {
		return
	}
	original, e := receiveCopy(path, c, m.Size)
	if e != nil {
		replyErr(c, fail("the desktop could not receive %s", name).because(e).hint("%v", e))
		return
	}
	editErr := desktop.Edit(path, m.Line)
	f, e := os.Open(path)
	if e != nil {
		replyErr(c, fail("the edited copy of %s is gone on the desktop", name).because(e).hint("%v", e))
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		replyErr(c, e)
		return
	}
	edited, e := changedSince(f, original)
	if e != nil {
		replyErr(c, e)
		return
	}
	// From here on the copy holds work the user may have saved.
	keep = edited
	if editErr != nil {
		if !edited {
			replyErr(c, fail("the editor failed on the desktop and %s was not changed", name).because(editErr).hint("%v", editErr))
			return
		}
		replyErr(c, saved("the editor failed on the desktop; %s on the server was not changed", name).because(editErr).hint("%v", editErr))
		return
	}
	if !edited {
		_ = wire.Write(c, wire.Message{Op: "unchanged", Desktop: desktopLabel()})
		return
	}
	if !st.Mode().IsRegular() || st.Size() > files.MaxBytes {
		replyErr(c, saved("the edited %s is not a regular file of at most %s", name, humanSize(files.MaxBytes)))
		return
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		replyErr(c, saved("could not reread the edited %s", name).because(e))
		return
	}
	if e = wire.Write(c, wire.Message{Size: st.Size(), Path: path, Desktop: desktopLabel()}); e != nil {
		fmt.Fprintf(os.Stderr, "2o: edit of %s: remote gone; edits kept in %s\n", name, path)
		return
	}
	if _, e = io.CopyN(c, f, st.Size()); e != nil {
		fmt.Fprintf(os.Stderr, "2o: edit of %s: remote gone; edits kept in %s\n", name, path)
		return
	}
	ack, e := wire.Read(c)
	if e != nil || ack.Error != "" {
		fmt.Fprintf(os.Stderr, "2o: edit of %s not applied on the server; edits kept in %s\n", name, path)
		return
	}
	keep = false
	f.Close()
	_ = os.RemoveAll(dir)
	ok(c) // the copy is gone: the remote may report success
}

// receiveCopy writes exactly size bytes from r into a new private file and
// returns their hash.
func receiveCopy(path string, r io.Reader, size int64) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return sum, err
	}
	h := sha256.New()
	_, err = io.CopyN(io.MultiWriter(f, h), r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	copy(sum[:], h.Sum(nil))
	return sum, err
}

// changedSince reports whether f's content no longer hashes to original.
func changedSince(f *os.File, original [sha256.Size]byte) (bool, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return !bytes.Equal(h.Sum(nil), original[:]), nil
}
