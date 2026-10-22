package main

import (
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/proxy"
	"github.com/arthurnbr/2o/internal/wire"
)

func response(c net.Conn) error {
	m, e := wire.Read(c)
	if e != nil {
		return e
	}
	if m.Error != "" {
		return errors.New(m.Error)
	}
	return nil
}
func send(m wire.Message) error {
	c, e := request(m)
	if e != nil {
		return e
	}
	defer c.Close()
	return response(c)
}
func httpURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
func localURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
func open(target string) error {
	if httpURL(target) {
		if os.Getenv("SSH_CONNECTION") == "" && os.Getenv("TWO_O_SESSION") == "" {
			return desktop.Open(target)
		}
		u, _ := url.Parse(target)
		if (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") && u.Scheme != "http" {
			return errors.New("remote localhost HTTPS is not supported")
		}
		if localURL(target) {
			c, e := request(wire.Message{Op: "proxy", URL: target})
			if e != nil {
				return e
			}
			defer c.Close()
			m, e := wire.Read(c)
			if e != nil {
				return e
			}
			if m.Error != "" {
				return errors.New(m.Error)
			}
			return proxy.Remote(c, target)
		}
		return send(wire.Message{Op: "open-url", URL: target})
	}
	if strings.Contains(target, "://") {
		return errors.New("unsupported URL scheme")
	}
	if os.Getenv("SSH_CONNECTION") == "" && os.Getenv("TWO_O_SESSION") == "" {
		return desktop.Open(target)
	}
	name, mode, size, r, err := files.Prepare(target)
	if err != nil {
		return err
	}
	defer r.Close()
	c, err := request(wire.Message{Op: "open-file", Name: name, Mode: uint32(mode), Size: size})
	if err != nil {
		return err
	}
	defer c.Close()
	if err = awaitAccept(c); err != nil {
		return err
	}
	if _, err = io.CopyN(c, r, size); err != nil {
		return err
	}
	return response(c)
}

// awaitAccept reads the desktop's go-ahead (or refusal) before the payload.
func awaitAccept(c net.Conn) error {
	m, err := wire.Read(c)
	if err != nil {
		return err
	}
	if m.Error != "" {
		return errors.New(m.Error)
	}
	if m.Op != "accept" {
		return errors.New("desktop daemon is out of date; restart it (pkill -f '2o daemon')")
	}
	return nil
}

func editFile(path string) error {
	original, mode, size, err := files.Snapshot(path)
	if err != nil {
		return err
	}
	if size > files.MaxBytes {
		return errors.New("file too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := request(wire.Message{Op: "edit", Name: filepath.Base(path), Size: size, Mode: uint32(mode), Hash: hex.EncodeToString(original[:])})
	if err != nil {
		return err
	}
	defer c.Close()
	if err = awaitAccept(c); err != nil {
		return err
	}
	if _, err = io.CopyN(c, f, size); err != nil {
		return err
	}
	m, err := wire.Read(c)
	if err != nil {
		return err
	}
	if m.Error != "" {
		return errors.New(m.Error)
	}
	if m.Size < 0 || m.Size > files.MaxBytes {
		return errors.New("edited file too large")
	}
	commitErr := files.Commit(path, original, c, m.Size)
	ack := wire.Message{}
	if commitErr != nil {
		ack.Error = commitErr.Error()
	}
	if err = wire.Write(c, ack); err != nil {
		return err
	}
	final, err := wire.Read(c)
	if err != nil {
		return err
	}
	if final.Error != "" {
		return errors.New(final.Error)
	}
	return commitErr
}
