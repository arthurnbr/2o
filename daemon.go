package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/arthurnbr/2o/internal/wire"
)

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
	go cacheJanitor()
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
