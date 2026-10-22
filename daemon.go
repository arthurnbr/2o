package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

// maxConns caps concurrent TCP connections, handshakes included; a var for tests.
var maxConns = 128

// idleTimeout drops an authenticated transfer that made no progress; a var for tests.
var idleTimeout = 60 * time.Second

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
	return filepath.Join(base, "2o-v3.sock"), nil
}
func keyPath(sock string) string  { return strings.TrimSuffix(sock, ".sock") + ".key" }
func portPath(sock string) string { return strings.TrimSuffix(sock, ".sock") + ".port" }

// desktopLabel names this desktop (user@hostname) for routing and status.
var desktopLabel = sync.OnceValue(func() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name + "@" + host
})

func daemon() error {
	path, err := socketPath()
	if err != nil {
		return err
	}
	d, err := openDaemon(path)
	if err != nil {
		return err
	}
	go cacheJanitor()
	return d.serve()
}

// desktopDaemon is a daemon bound to its control socket and loopback port.
type desktopDaemon struct {
	sock   string
	ctl    net.Listener // master-authenticated holds (Unix socket, mode 0600)
	tcp    net.Listener // session-authenticated requests from SSH forwards
	reg    *registry
	closed sync.Once
}

// openDaemon binds the control socket at sock, the stable loopback port, and
// writes the key file `master addr` next to the socket.
func openDaemon(sock string) (*desktopDaemon, error) {
	if st, e := os.Lstat(sock); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("daemon socket path occupied")
		}
		c, e := net.DialTimeout("unix", sock, dialTimeout)
		if e == nil {
			c.Close()
			return nil, errors.New("daemon already running")
		}
		if e = os.Remove(sock); e != nil {
			return nil, e
		}
	}
	master, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	ctl, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	d := &desktopDaemon{sock: sock, ctl: ctl, reg: newRegistry(master)}
	if err = os.Chmod(sock, 0600); err == nil {
		// SSH remote forwards target TCP on the desktop: the loopback
		// listener only answers sessions authenticated with a held secret.
		d.tcp, err = listenStable(portPath(sock))
	}
	if err == nil {
		err = writePrivate(keyPath(sock), []byte(master+" "+d.tcp.Addr().String()+"\n"))
	}
	if err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

// close stops accepting and removes the socket and key; serve then revokes
// every hold and returns. Only the first call acts, so a late close never
// removes the files of a daemon started since.
func (d *desktopDaemon) close() {
	d.closed.Do(func() {
		_ = os.Remove(keyPath(d.sock))
		_ = os.Remove(d.sock)
		d.ctl.Close()
		if d.tcp != nil {
			d.tcp.Close()
		}
	})
}

// serve runs the daemon loop until the control socket stops accepting. On
// return every hold is revoked, which closes all session connections.
func (d *desktopDaemon) serve() error {
	defer d.reg.revokeAll()
	defer d.close()
	slots := make(chan struct{}, maxConns)
	go func() {
		for {
			c, err := d.tcp.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					serveTCP(d.reg, c)
				}()
			default:
				go refuseBusy(c)
			}
		}
	}()
	for {
		c, err := d.ctl.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go serveControl(d.reg, c)
	}
}

// listenStable binds the loopback port recorded in portFile when it is free,
// so SSH forwards made before a daemon restart still reach the new daemon;
// otherwise it picks a new port and records it.
func listenStable(portFile string) (net.Listener, error) {
	if b, err := readPrivate(portFile); err == nil {
		if port, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && port > 0 && port < 65536 {
			if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
				return l, nil
			}
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err = writePrivate(portFile, []byte(port+"\n")); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func serveTCP(reg *registry, c net.Conn) {
	defer c.Close()
	m, h, err := reg.authenticate(c)
	if err != nil {
		_ = wire.Write(c, wire.Message{Error: err.Error()})
		return
	}
	defer reg.detach(h, c)
	// The bridge's connections idle by design for as long as it runs.
	if m.Op == "proxy" || m.Op == "proxy-stream" {
		serve(c, m)
		return
	}
	serve(&idleConn{Conn: c, idle: idleTimeout}, m)
}

// refuseBusy answers a connection over the cap after reading its hello, so
// the reply is not lost to a reset.
func refuseBusy(c net.Conn) {
	defer c.Close()
	if c.SetDeadline(time.Now().Add(2*time.Second)) != nil {
		return
	}
	if _, err := wire.Read(c); err != nil {
		return
	}
	_ = wire.Write(c, wire.Message{
		Error: "desktop daemon is busy",
		Hints: []string{fmt.Sprintf("%d transfers are already running; retry when some finish", maxConns)},
		Code:  exitNoTunnel,
	})
}

func serveControl(reg *registry, c net.Conn) {
	defer c.Close()
	if c.SetDeadline(time.Now().Add(handshakeTimeout)) != nil {
		return
	}
	m, err := wire.Read(c)
	if err == nil {
		if m.Op == "hold" {
			err = reg.holdControl(c, m)
		} else {
			err = errUnauthorized
		}
	}
	if err != nil {
		_ = wire.Write(c, wire.Message{Error: err.Error()})
	}
}

// idleConn renews a deadline on every read and write: a transfer may take
// any time while it makes progress, but a stalled peer is dropped after idle.
// Waiting on the desktop (editor, confirmation) does no I/O and is not limited.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
