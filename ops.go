package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/proxy"
	"github.com/arthurnbr/2o/internal/wire"
)

func serve(c net.Conn, m wire.Message) {
	var err error
	fail := func(err error) { _ = wire.Write(c, wire.Message{Error: err.Error()}) }
	switch m.Op {
	case "ping":
		_ = wire.Write(c, wire.Message{})
	case "open-url":
		if !httpURL(m.URL) {
			fail(errors.New("only HTTP(S) URLs allowed"))
			return
		}
		if err = desktop.Open(m.URL); err != nil {
			fail(err)
			return
		}
		_ = wire.Write(c, wire.Message{})
	case "open-file":
		if m.Size < 0 || m.Size > files.MaxBytes {
			fail(errors.New("file too large"))
			return
		}
		base := cacheDir()
		if err = os.MkdirAll(base, 0700); err != nil {
			fail(err)
			return
		}
		if err = admitTransfer(m.Name, m.Size, base); err != nil {
			fail(err)
			return
		}
		// The remote waits for this go-ahead before streaming any payload.
		if err = wire.Write(c, wire.Message{Op: "accept"}); err != nil {
			return
		}
		path, e := files.Receive(m.Name, os.FileMode(m.Mode), m.Size, c, base)
		if e != nil {
			fail(e)
			return
		}
		if err = desktop.Open(path); err != nil {
			fail(err)
			return
		}
		_ = wire.Write(c, wire.Message{})
	case "copy":
		if m.Size < 0 || m.Size > clipLimit() {
			fail(errors.New("clipboard too large"))
			return
		}
		b, e := io.ReadAll(io.LimitReader(c, m.Size))
		if e != nil {
			fail(e)
			return
		}
		if int64(len(b)) != m.Size {
			fail(io.ErrUnexpectedEOF)
			return
		}
		if err = desktop.Copy(b); err != nil {
			fail(err)
			return
		}
		_ = wire.Write(c, wire.Message{})
	case "paste":
		b, e := desktop.Paste()
		if e != nil {
			fail(e)
			return
		}
		if int64(len(b)) > clipLimit() {
			fail(errors.New("clipboard too large"))
			return
		}
		if err = wire.Write(c, wire.Message{Size: int64(len(b))}); err == nil {
			_, _ = c.Write(b)
		}
	case "notify":
		if len(m.Title) > 512 || len(m.Body) > 4096 {
			fail(errors.New("notification too long"))
			return
		}
		if err = desktop.Notify(m.Title, m.Body); err != nil {
			fail(err)
			return
		}
		_ = wire.Write(c, wire.Message{})
	case "edit":
		serveEdit(c, m)
	case "proxy":
		if !localURL(m.URL) {
			fail(errors.New("proxy accepts only loopback HTTP"))
			return
		}
		err = proxy.Local(c, m.URL, func(u string) error {
			if e := wire.Write(c, wire.Message{URL: u}); e != nil {
				return e
			}
			return desktop.Open(u)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "2o proxy:", err)
		}
	default:
		fail(errors.New("unknown operation"))
	}
}

func serveEdit(c net.Conn, m wire.Message) {
	fail := func(e error) { _ = wire.Write(c, wire.Message{Error: e.Error()}) }
	if m.Size < 0 || m.Size > files.MaxBytes || strings.ContainsAny(m.Name, "/\\\x00") {
		fail(errors.New("invalid edit file"))
		return
	}
	if e := os.MkdirAll(cacheDir(), 0700); e != nil {
		fail(e)
		return
	}
	if e := admitTransfer(m.Name, m.Size, cacheDir()); e != nil {
		fail(e)
		return
	}
	if e := wire.Write(c, wire.Message{Op: "accept"}); e != nil {
		return
	}
	dir, e := os.MkdirTemp(cacheDir(), "edit-")
	if e != nil {
		fail(e)
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, m.Name)
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		fail(e)
		return
	}
	_, e = io.CopyN(f, c, m.Size)
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		fail(e)
		return
	}
	if e = desktop.Edit(path); e != nil {
		fail(e)
		return
	}
	f, e = os.Open(path)
	if e != nil {
		fail(e)
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		fail(e)
		return
	}
	if !st.Mode().IsRegular() || st.Size() > files.MaxBytes {
		fail(errors.New("edited file invalid or too large"))
		return
	}
	keep = true // Preserve the edited copy unless the remote confirms its write.
	if e = wire.Write(c, wire.Message{Size: st.Size()}); e != nil {
		return
	}
	if _, e = io.CopyN(c, f, st.Size()); e != nil {
		return
	}
	ack, e := wire.Read(c)
	if e != nil {
		fmt.Fprintln(os.Stderr, "2o: edit disconnected; recovery copy:", path)
		return
	}
	if ack.Error != "" {
		_ = wire.Write(c, wire.Message{Error: ack.Error + "; recovery copy: " + path})
		return
	}
	keep = false
	_ = wire.Write(c, wire.Message{})
}
