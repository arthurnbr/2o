package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/proxy"
	"github.com/arthurnbr/2o/internal/ui"
	"github.com/arthurnbr/2o/internal/wire"
)

// remoteErr turns a daemon reply carrying an error into a *userError with the
// daemon's hints and exit code; nil when the reply is not an error.
func remoteErr(m wire.Message) error {
	if m.Error == "" {
		return nil
	}
	code := m.Code
	if code == 0 {
		code = 1
	}
	return &userError{msg: m.Error, hints: m.Hints, code: code}
}

// lostDesktop explains a connection that broke in the middle of a request.
func lostDesktop(where, doing string, err error) *userError {
	return fail("lost the connection to %s while %s", where, doing).because(err).
		hint("check the tunnel with: 2o status").exit(exitNoTunnel)
}

func response(c net.Conn) error {
	m, err := wire.Read(c)
	if err != nil {
		return lostDesktop("the desktop", "waiting for its answer", err)
	}
	return remoteErr(m)
}

func send(m wire.Message) error {
	c, err := request(m)
	if err != nil {
		return err
	}
	defer c.Close()
	return response(c)
}

func httpURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// remote reports whether this shell talks to a desktop through 2o; otherwise
// 2o runs on the desktop itself and opens things directly.
func remote() bool { return os.Getenv("SSH_CONNECTION") != "" || os.Getenv("TWO_O_SESSION") != "" }

// targetDesktop names the desktop this shell's requests go to, for messages.
func targetDesktop() string {
	if s, _ := currentSession(); s.desktop != "" {
		return s.desktop
	}
	return "the desktop"
}

func open(target string) error {
	if httpURL(target) {
		if !remote() {
			return openLocally(target)
		}
		u, _ := url.Parse(target)
		if proxy.IsLoopback(u.Hostname()) {
			return openBridge(target)
		}
		where := targetDesktop()
		s := ui.Start("opening %s on %s", target, where)
		if err := send(wire.Message{Op: "open-url", URL: target}); err != nil {
			s.Fail()
			return err
		}
		s.DoneAs("opened %s on %s", target, where)
		return nil
	}
	if strings.Contains(target, "://") {
		return fail("cannot open %s: only http and https URLs are supported", target).exit(exitUsage)
	}
	if !remote() {
		return openLocally(target)
	}
	return sendFile(target)
}

func openLocally(target string) error {
	if err := desktop.Open(target); err != nil {
		return fail("could not open %s", target).because(err).hint("%v", err)
	}
	return nil
}

// sendFile copies a file or directory to the desktop, which opens it.
func sendFile(target string) error {
	name, mode, size, r, err := files.Prepare(target)
	if err != nil {
		return fileError(target, err)
	}
	defer r.Close()
	where := targetDesktop()
	s := ui.Start("sending %s (%s) to %s", name, humanSize(size), where)
	c, err := request(wire.Message{Op: "open-file", Name: name, Mode: uint32(mode), Size: size})
	if err != nil {
		s.Fail()
		return err
	}
	defer c.Close()
	if _, where, err = awaitAccept(c, where, s.Warn); err != nil {
		s.Fail()
		return err
	}
	var src io.Reader = r
	if ui.Live() {
		src = io.TeeReader(r, &progress{step: s, total: size, label: func(p string) string {
			return fmt.Sprintf("sending %s to %s — %s", name, where, p)
		}})
	}
	w := &connWriter{c: c}
	if _, err = io.CopyN(w, src, size); err != nil {
		s.Fail()
		if w.err == nil {
			return fail("could not read %s while sending it", target).because(err).hint("%v", err)
		}
		// The desktop may have stopped reading because it refused the data.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if m, rerr := wire.Read(c); rerr == nil && m.Error != "" {
			return remoteErr(m)
		}
		return lostDesktop(where, "sending "+name, err)
	}
	if err = response(c); err != nil {
		s.Fail()
		return err
	}
	s.DoneAs("opened %s on %s", name, where)
	return nil
}

// connWriter remembers whether a copy failed on the connection side.
type connWriter struct {
	c   net.Conn
	err error
}

func (w *connWriter) Write(b []byte) (int, error) {
	n, err := w.c.Write(b)
	if err != nil {
		w.err = err
	}
	return n, err
}

func fileError(target string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fail("%s does not exist", target).because(err).exit(exitUsage)
	}
	return fail("cannot send %s", target).because(err).hint("%v", err)
}

// awaitAccept reads the desktop's go-ahead (or refusal) before the payload.
// While the desktop asks its user to confirm, warn says so. It returns the
// accept message and the desktop's own label when the reply carries one.
func awaitAccept(c net.Conn, where string, warn func(string, ...any)) (wire.Message, string, error) {
	for {
		m, err := wire.Read(c)
		if err != nil {
			return m, where, lostDesktop(where, "waiting for it to accept", err)
		}
		if m.Desktop != "" {
			where = m.Desktop
		}
		if err = remoteErr(m); err != nil {
			return m, where, err
		}
		switch m.Op {
		case "waiting":
			warn("waiting for confirmation on %s…", where)
		case "accept":
			return m, where, nil
		default:
			return m, where, fail("the desktop daemon is older than this 2o (%s)", version).
				hint("restart it on the desktop: 2o ssh reconnects with the new one").exit(exitUsage)
		}
	}
}

// confirmCommit tells the desktop the edit was written, and waits until it
// dropped its copy.
func confirmCommit(c net.Conn) error {
	if err := wire.Write(c, wire.Message{}); err != nil {
		return err
	}
	return response(c)
}

// progress feeds a step's label with the share of bytes sent, a few times a second.
type progress struct {
	step        *ui.Step
	total, done int64
	last        time.Time
	label       func(string) string
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if now := time.Now(); now.Sub(p.last) >= 150*time.Millisecond || p.done == p.total {
		p.last = now
		pct := int64(100)
		if p.total > 0 {
			pct = p.done * 100 / p.total
		}
		p.step.Update("%s", p.label(fmt.Sprintf("%d%% (%s of %s)", pct, humanSize(p.done), humanSize(p.total))))
	}
	return len(b), nil
}

// editTarget splits FILE:LINE. A path that exists as written wins, so
// "notes:12" is a file when such a file exists.
func editTarget(arg string) (path string, line int) {
	i := strings.LastIndexByte(arg, ':')
	if i <= 0 || i == len(arg)-1 {
		return arg, 0
	}
	if _, err := os.Lstat(arg); err == nil {
		return arg, 0
	}
	n := 0
	for _, r := range arg[i+1:] {
		if r < '0' || r > '9' || n > 1e8 {
			return arg, 0
		}
		n = n*10 + int(r-'0')
	}
	return arg[:i], n
}

// editFile edits path on the desktop and writes the result back only if the
// file did not change meanwhile. Whenever the result cannot be written, the
// desktop keeps the edited copy and the error says where.
func editFile(path string, line int, create bool) error {
	name := filepath.Base(path)
	var f *os.File
	var size int64
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && create:
		if _, err = os.Stat(filepath.Dir(path)); err != nil {
			return fail("cannot create %s: %s does not exist", path, filepath.Dir(path)).because(err).exit(exitUsage)
		}
	case errors.Is(err, fs.ErrNotExist):
		return fail("%s does not exist", path).because(err).
			hint("create it with: 2o edit -n %s", path).exit(exitUsage)
	case err != nil:
		return fail("cannot edit %s", path).because(err).hint("%v", err)
	case !info.Mode().IsRegular():
		return fail("cannot edit %s: not a regular file", path).exit(exitUsage)
	default:
		// Open without following links and check it is still the file just
		// inspected: what is sent is exactly what the conflict check compares.
		if f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0); err != nil {
			return fail("cannot read %s", path).because(err).hint("%v", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return fail("cannot read %s", path).because(err).hint("%v", err)
		}
		if !os.SameFile(info, st) || st.Size() != info.Size() {
			return fail("%s changed while 2o was opening it", path).hint("run the command again")
		}
		size = st.Size()
		create = false
	}
	where := targetDesktop()
	c, err := request(wire.Message{Op: "edit", Name: name, Size: size, Line: line})
	if err != nil {
		return err
	}
	defer c.Close()
	accept, where, err := awaitAccept(c, where, ui.Warn)
	if err != nil {
		return err
	}
	h := sha256.New()
	if f != nil {
		if _, err = io.CopyN(c, io.TeeReader(f, h), size); err != nil {
			return lostDesktop(where, "sending "+name, err)
		}
	}
	var original [sha256.Size]byte
	copy(original[:], h.Sum(nil))
	ui.Info("editing %s on %s — save and close the editor to apply", name, ui.Bold(where))
	m, err := wire.Read(c)
	if err != nil {
		return lostDesktop(where, "you were editing "+name, err).
			hint("if you saved, your edits are safe in %s on %s", accept.Path, where)
	}
	if err = remoteErr(m); err != nil {
		return err
	}
	if m.Op == "unchanged" {
		if create {
			ui.Info("no changes; %s was not created", name)
		} else {
			ui.Info("no changes to %s", name)
		}
		return nil
	}
	saved := fmt.Sprintf("your edits are safe in %s on %s", m.Path, where)
	if m.Size < 0 || m.Size > files.MaxBytes {
		_ = wire.Write(c, wire.Message{Error: "invalid size"})
		return fail("the desktop sent an invalid size for %s", name).hint("%s", saved)
	}
	if create {
		err = files.Create(path, c, m.Size, 0666)
	} else {
		err = files.Commit(path, original, c, m.Size)
	}
	if err != nil {
		_ = wire.Write(c, wire.Message{Error: err.Error()})
		if errors.Is(err, files.ErrConflict) {
			what := "changed"
			if create {
				what = "was created"
			}
			return fail("%s %s on the server while you were editing; it was not overwritten", path, what).because(err).
				hint("%s", saved).hint("merge your edits, then run 2o edit again")
		}
		return fail("could not write %s", path).because(err).hint("%v", err).hint("%s", saved)
	}
	if err = confirmCommit(c); err != nil {
		// Written here; the desktop just keeps a copy it did not need to.
		ui.Warn("%s updated, but the desktop missed the confirmation and keeps a copy in %s", name, m.Path)
		return nil
	}
	if create {
		ui.Success("%s created", name)
	} else {
		ui.Success("%s updated", name)
	}
	return nil
}
