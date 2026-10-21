package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/proxy"
	"github.com/arthurnbr/2o/internal/wire"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "2o:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: 2o daemon|ssh HOST|shell|open URL|FILE|copy|paste|edit FILE|notify TITLE BODY")
	}
	if filepath.Base(os.Args[0]) == "xdg-open" || filepath.Base(os.Args[0]) == "open" {
		if len(args) != 1 {
			return errors.New("usage: xdg-open URL|FILE")
		}
		return open(args[0])
	}
	switch args[0] {
	case "daemon":
		return daemon()
	case "ssh":
		return sshSession(args[1:])
	case "free-port":
		port, err := freePort()
		if err != nil {
			return err
		}
		fmt.Println(port)
		return nil
	case "status":
		if len(args) != 1 {
			return errors.New("usage: 2o status")
		}
		c, err := request(wire.Message{Op: "ping"})
		if err != nil {
			return err
		}
		defer c.Close()
		if err = response(c); err != nil {
			return err
		}
		fmt.Println("2o: desktop tunnel active")
		return nil
	case "shell":
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		return syscall.Exec(shell, []string{shell, "-l"}, os.Environ())
	case "open":
		if len(args) != 2 {
			return errors.New("usage: 2o open URL|FILE")
		}
		return open(args[1])
	case "copy":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, files.MaxBytes+1))
		if err != nil {
			return err
		}
		if len(b) > int(files.MaxBytes) {
			return errors.New("clipboard too large")
		}
		c, err := request(wire.Message{Op: "copy", Size: int64(len(b))})
		if err != nil {
			if os.Getenv("SSH_CONNECTION") != "" {
				return desktop.OSC52(b, os.Stdout)
			}
			return err
		}
		defer c.Close()
		if _, err = c.Write(b); err != nil {
			return err
		}
		return response(c)
	case "paste":
		c, err := request(wire.Message{Op: "paste"})
		if err != nil {
			return err
		}
		defer c.Close()
		m, err := wire.Read(c)
		if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.Size < 0 || m.Size > files.MaxBytes {
			return errors.New("clipboard too large")
		}
		_, err = io.CopyN(os.Stdout, c, m.Size)
		return err
	case "notify":
		if len(args) != 3 {
			return errors.New("usage: 2o notify TITLE BODY")
		}
		return send(wire.Message{Op: "notify", Title: args[1], Body: args[2]})
	case "edit":
		if len(args) != 2 {
			return errors.New("usage: 2o edit FILE")
		}
		return editFile(args[1])
	default:
		if len(args) == 1 {
			return open(args[0])
		}
		return errors.New("unknown command")
	}
}

// socketPath is versioned so a daemon from an incompatible release can keep
// running without blocking this one.
func socketPath() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("2o-%d", os.Getuid()))
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(base)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("unsafe daemon socket directory")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return "", errors.New("daemon socket directory belongs to another user")
	}
	return filepath.Join(base, "2o-v2.sock"), nil
}
func keyPath(sock string) string { return strings.TrimSuffix(sock, ".sock") + ".key" }
func daemon() error {
	path, err := socketPath()
	if err != nil {
		return err
	}
	if st, e := os.Lstat(path); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("daemon socket path occupied")
		}
		c, e := net.Dial("unix", path)
		if e == nil {
			c.Close()
			return errors.New("daemon already running")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	master, err := randomHex(32)
	if err != nil {
		return err
	}
	// SSH remote forwards target TCP on the desktop: the loopback listener
	// only answers sessions authenticated with a registered secret.
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer tl.Close()
	key := keyPath(path)
	_ = os.Remove(key)
	if err = os.WriteFile(key, []byte(master+" "+tl.Addr().String()+"\n"), 0600); err != nil {
		return err
	}
	defer os.Remove(key)
	reg := newRegistry(master)
	handle := func(c net.Conn) {
		defer c.Close()
		m, err := reg.authenticate(c)
		if err != nil {
			_ = wire.Write(c, wire.Message{Error: err.Error()})
			return
		}
		if m.Op != "register" && m.Op != "unregister" {
			serve(c, m)
		}
	}
	go func() {
		for {
			c, e := tl.Accept()
			if e != nil {
				return
			}
			go handle(c)
		}
	}()
	for {
		c, e := l.Accept()
		if e != nil {
			return e
		}
		go handle(c)
	}
}
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
		if m.Size < 0 || m.Size > files.MaxBytes {
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
		if len(b) > int(files.MaxBytes) {
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
	if _, err = io.CopyN(c, r, size); err != nil {
		return err
	}
	return response(c)
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
