package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

// The remote end reaches the desktop daemon through an SSH remote TCP forward
// bound to the server's loopback. Unix-socket forwards are not used: Tailscale
// SSH creates them as root with mode 0600, so the logged-in user cannot connect.
// Any local user on the server can reach a loopback port, so each connection
// performs mutual authentication with a per-session secret before any data
// flows:
//
//	client → hello{Name: id, Hash: clientNonce}
//	daemon → {Hash: serverProof, Token: serverNonce}   (fresh per connection)
//	client → request header with Token = clientProof
//
// serverProof = HMAC(token, "server", id, clientNonce, serverNonce) is checked
// before anything is sent, so an impostor bound to a stale port never receives
// file contents. clientProof = HMAC(token, "client", id, clientNonce,
// serverNonce, canonical request header) binds the proof to this connection's
// server nonce (a captured exchange cannot be replayed) and to the request
// itself (it cannot be altered after the proof).

// sessionGlob finds this user's remote session files; a var for tests.
var sessionGlob = "/tmp/2o-%d-*/session"

const (
	nonceLen         = 32 // hex characters of a handshake nonce
	dialTimeout      = 3 * time.Second
	handshakeTimeout = 10 * time.Second
)

var (
	errUnauthorized  = errors.New("unauthorized")
	errClosedSession = errors.New("unknown or closed 2o session")
	errUnsafeFile    = errors.New("not private to this user")
)

// session is one remote end's view of a desktop: the session file
// `addr id token [desktop]` written by `2o ssh`.
type session struct {
	path    string // session file
	addr    string // server-side loopback end of the SSH forward
	id      string
	token   string
	desktop string // user@host of the desktop; empty in older 3-field files
}

// hold is a session registered by a live control connection (`2o ssh`). It is
// valid exactly as long as that connection stays open.
type hold struct {
	token   string
	desktop string
	ctl     net.Conn              // the holder's control connection
	conns   map[net.Conn]struct{} // authenticated connections of this session
	closed  bool
}

type registry struct {
	master string
	mu     sync.Mutex
	holds  map[string]*hold
}

func newRegistry(master string) *registry {
	return &registry{master: master, holds: make(map[string]*hold)}
}

func (r *registry) lookup(id string) *hold {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holds[id]
}

func (r *registry) add(id string, h *hold) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.holds[id]; ok {
		return errors.New("session already held")
	}
	r.holds[id] = h
	return nil
}

// attach records c as an active connection of h. It fails once h is revoked,
// so a handshake that looked the token up before revocation stops here.
func (r *registry) attach(h *hold, c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (r *registry) detach(h *hold, c net.Conn) {
	r.mu.Lock()
	delete(h.conns, c)
	r.mu.Unlock()
}

// revoke forgets h and closes its control connection and every active
// connection of the session.
func (r *registry) revoke(id string, h *hold) {
	r.mu.Lock()
	if r.holds[id] == h {
		delete(r.holds, id)
	}
	h.closed = true
	conns := h.conns
	h.conns = nil
	r.mu.Unlock()
	h.ctl.Close()
	for c := range conns {
		c.Close()
	}
}

func (r *registry) revokeAll() {
	r.mu.Lock()
	holds := make(map[string]*hold, len(r.holds))
	for id, h := range r.holds {
		holds[id] = h
	}
	r.mu.Unlock()
	for id, h := range holds {
		r.revoke(id, h)
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// mac is HMAC-SHA256 over length-prefixed parts, so no two part lists collide.
func mac(token string, parts ...string) string {
	h := hmac.New(sha256.New, []byte(token))
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		io.WriteString(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serverProof(token, id, clientNonce, serverNonce string) string {
	return mac(token, "2o-v3 server", id, clientNonce, serverNonce)
}

// clientProof binds the canonical request header: the header's JSON encoding
// without the proof itself, so every field the daemon may act on (op, name,
// size, mode, hash, url, title, body, and any later addition) is covered.
// Both ends encode the same struct, and json.Marshal is deterministic and
// cannot fail for it. Fields a daemon does not know are dropped when it
// decodes, so their proof fails: unknown fields are refused, not ignored.
func clientProof(token, id, clientNonce, serverNonce string, m wire.Message) string {
	m.Token = ""
	header, _ := json.Marshal(m)
	return mac(token, "2o-v3 client", id, clientNonce, serverNonce, string(header))
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// privateFile opens path for reading only if it is a regular file owned by
// the caller with no group/other permissions, inside a directory with the same
// properties. Neither may be a symlink (O_NOFOLLOW), and the checks fstat the
// opened descriptors, so nothing can be swapped between check and read. The
// directory's parent may be /tmp: its sticky bit stops other users from
// replacing an entry we own.
func privateFile(path string) (*os.File, error) {
	dir, err := openNoFollow(filepath.Dir(path), syscall.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	err = checkPrivate(dir, true)
	dir.Close()
	if err != nil {
		return nil, err
	}
	f, err := openNoFollow(path, 0)
	if err != nil {
		return nil, err
	}
	if err = checkPrivate(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openNoFollow(path string, flag int) (*os.File, error) {
	// O_NONBLOCK: opening a FIFO planted in place of the file must not hang.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|flag, 0)
	// A symlink fails with ELOOP, or ENOTDIR when a directory was required.
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return nil, fmt.Errorf("%s is a symbolic link or not a directory: %w", path, errUnsafeFile)
	}
	return f, err
}

func checkPrivate(f *os.File, dir bool) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s: %w", f.Name(), errUnsafeFile)
	}
	if dir != st.IsDir() || !dir && !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file: %w", f.Name(), errUnsafeFile)
	}
	return nil
}

// readPrivate reads a small private file (session, key, port).
func readPrivate(path string) ([]byte, error) {
	f, err := privateFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4096))
}

// writePrivate atomically replaces path with a new 0600 file.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func reconnectHint(e *userError) *userError {
	return e.hint("reconnect from your desktop with: 2o ssh HOST").exit(exitNoTunnel)
}

func readSession(path string) (session, error) {
	b, err := readPrivate(path)
	if errors.Is(err, errUnsafeFile) {
		return session{path: path}, reconnectHint(fail("unsafe session file %s", path).
			hint("it must be a regular file owned by you, mode 0600, in a private directory").because(err))
	}
	f := strings.Fields(string(b))
	if err == nil && len(f) != 3 && len(f) != 4 {
		err = fmt.Errorf("malformed session file %s", path)
	}
	if err != nil {
		return session{path: path}, reconnectHint(fail("desktop tunnel unavailable").because(err))
	}
	s := session{path: path, addr: f[0], id: f[1], token: f[2]}
	if len(f) == 4 {
		s.desktop = f[3]
	}
	return s, nil
}

// currentSession is the session this shell targets ($TWO_O_SESSION), with no
// fallback. On error, s.path is still the configured path (or empty) and
// s.desktop is $TWO_O_DESKTOP, which `2o ssh` exports so a shell whose session
// file is gone still knows which desktop it belonged to.
func currentSession() (session, error) {
	p := os.Getenv("TWO_O_SESSION")
	if p == "" {
		return session{}, fail("no desktop tunnel in this shell").
			hint("connect from your desktop with: 2o ssh HOST").exit(exitNoTunnel)
	}
	s, err := readSession(p)
	if s.desktop == "" {
		s.desktop = os.Getenv("TWO_O_DESKTOP")
	}
	return s, err
}

// replyError turns a daemon refusal into an error; replies carrying an exit
// code are already explained for humans.
func replyError(m wire.Message) error {
	if m.Code != 0 {
		return &userError{msg: m.Error, hints: m.Hints, code: m.Code}
	}
	return errors.New(m.Error)
}

// dialSession authenticates both ends, then sends m as the first request.
func dialSession(s session, m wire.Message) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", s.addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if err = c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fail(err)
	}
	nonce, err := randomHex(nonceLen / 2)
	if err != nil {
		return fail(err)
	}
	if err = wire.Write(c, wire.Message{Op: "hello", Name: s.id, Hash: nonce}); err != nil {
		return fail(err)
	}
	reply, err := wire.Read(c)
	if err != nil {
		return fail(fmt.Errorf("desktop daemon did not answer: %w", err))
	}
	if reply.Error != "" {
		return fail(replyError(reply))
	}
	if len(reply.Token) != nonceLen || !equal(reply.Hash, serverProof(s.token, s.id, nonce, reply.Token)) {
		return fail(errors.New("endpoint failed authentication; refusing to send data"))
	}
	m.Token = clientProof(s.token, s.id, nonce, reply.Token, m)
	if err = wire.Write(c, m); err != nil {
		return fail(err)
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return c, nil
}

// request sends m to this shell's desktop. If its session is gone (e.g. a
// reattached tmux kept an old TWO_O_SESSION) it moves on only to the single
// live session of the same desktop; anything else is the user's choice.
func request(m wire.Message) (net.Conn, error) {
	cur, err := currentSession()
	if err == nil {
		c, e := dialSession(cur, m)
		if e == nil {
			return c, nil
		}
		var ue *userError
		if errors.As(e, &ue) {
			return nil, e // the desktop answered and refused
		}
		err = e
	} else if errors.Is(err, errUnsafeFile) {
		return nil, err
	}
	live := liveSessions(cur.path)
	if len(live) == 1 && cur.desktop != "" && live[0].desktop == cur.desktop {
		c, e := dialSession(live[0], m)
		if e == nil {
			return c, nil
		}
		var ue *userError
		if errors.As(e, &ue) {
			return nil, e
		}
		return nil, reconnectHint(fail("desktop tunnel unavailable").because(e))
	}
	return nil, routeError(cur, live, err)
}

// routeError explains why no session was used and how to pick one.
func routeError(cur session, live []session, cause error) error {
	var ue *userError
	if len(live) == 0 {
		if errors.As(cause, &ue) {
			return cause
		}
		return reconnectHint(fail("desktop tunnel unavailable").because(cause))
	}
	if cur.path == "" {
		ue = fail("no desktop tunnel in this shell")
	} else {
		ue = fail("this shell's desktop tunnel is closed")
	}
	ue.because(cause).hint("pick a live session:")
	for _, s := range live {
		label := s.desktop
		if label == "" {
			label = "unknown desktop"
		}
		ue.hint("export TWO_O_SESSION=%s   # %s", s.path, label)
	}
	return ue.hint("or reconnect from your desktop with: 2o ssh HOST").exit(exitNoTunnel)
}

// liveSessions lists this user's other session files whose desktop answers,
// newest first.
func liveSessions(except string) []session {
	matches, _ := filepath.Glob(fmt.Sprintf(sessionGlob, os.Getuid()))
	sort.Slice(matches, func(i, j int) bool { return modTime(matches[i]).After(modTime(matches[j])) })
	var live []session
	for _, p := range matches {
		if p == except {
			continue
		}
		s, err := readSession(p)
		if err != nil {
			continue
		}
		c, err := dialSession(s, wire.Message{Op: "ping"})
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
		reply, err := wire.Read(c)
		c.Close()
		if err == nil && reply.Error == "" {
			if s.desktop == "" {
				s.desktop = reply.Desktop
			}
			live = append(live, s)
		}
	}
	return live
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// authenticate runs the daemon side of the handshake on a TCP connection and
// returns the request with the session it is attached to; the caller detaches
// it when done.
func (r *registry) authenticate(c net.Conn) (wire.Message, *hold, error) {
	if err := c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return wire.Message{}, nil, err
	}
	hello, err := wire.Read(c)
	if err != nil {
		return hello, nil, err
	}
	if hello.Op != "hello" {
		return hello, nil, errUnauthorized
	}
	h := r.lookup(hello.Name)
	if h == nil || len(hello.Hash) != nonceLen {
		return hello, nil, errClosedSession
	}
	nonce, err := randomHex(nonceLen / 2)
	if err != nil {
		return hello, nil, err
	}
	if err = wire.Write(c, wire.Message{Hash: serverProof(h.token, hello.Name, hello.Hash, nonce), Token: nonce}); err != nil {
		return hello, nil, err
	}
	req, err := wire.Read(c)
	if err != nil {
		return req, nil, err
	}
	if !equal(req.Token, clientProof(h.token, hello.Name, hello.Hash, nonce, req)) {
		return req, nil, errUnauthorized
	}
	if !r.attach(h, c) {
		return req, nil, errClosedSession
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		r.detach(h, c)
		return req, nil, err
	}
	return req, h, nil
}

// holdControl serves Op "hold" on the Unix control socket: m.Name/m.Hash are
// registered while c stays open, then revoked with all their connections.
func (r *registry) holdControl(c net.Conn, m wire.Message) error {
	if !equal(m.Token, r.master) {
		return errUnauthorized
	}
	if m.Name == "" || len(m.Hash) < nonceLen {
		return errors.New("invalid session")
	}
	h := &hold{token: m.Hash, desktop: m.Desktop, ctl: c, conns: make(map[net.Conn]struct{})}
	if err := r.add(m.Name, h); err != nil {
		return err
	}
	defer r.revoke(m.Name, h)
	if wire.Write(c, wire.Message{Desktop: desktopLabel()}) != nil || c.SetDeadline(time.Time{}) != nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, c) // until the holder closes or dies
	return nil
}

// sessions is how many sessions are currently held.
func (r *registry) sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.holds)
}

// Errors reported to holdSession's notify callback.
var (
	errHoldLost   = errors.New("desktop daemon went away")
	errHoldFailed = errors.New("cannot restore the desktop session")
)

// restartDaemon starts (or finds) the desktop daemon and returns its socket.
var restartDaemon = ensureDaemon

// holdSession registers session id/token with the desktop daemon at sock for
// as long as the returned release is not called (idempotent; it revokes the
// token and closes the session's connections).
//
// If the daemon goes away (crash, restart), notify (nil-safe, called from a
// background goroutine) receives an error wrapping errHoldLost, and the hold
// is re-established on a restarted daemon: notify(nil) once held again. The
// daemon keeps its TCP port across restarts, so the SSH forward stays valid.
// If it cannot be restored within 30s, or the daemon came back on another
// port (the forward is then dead), notify receives an error wrapping
// errHoldFailed and holding stops: the session must be reconnected.
func holdSession(sock, master, id, token, desktop string, notify func(error)) (release func(), err error) {
	_, addr, err := readKey(keyPath(sock))
	if err != nil {
		return nil, err
	}
	c, err := dialHold(sock, master, id, token, desktop)
	if err != nil {
		return nil, err
	}
	if notify == nil {
		notify = func(error) {}
	}
	h := &holder{conn: c, done: make(chan struct{})}
	go h.watch(sock, addr, id, token, desktop, notify)
	return h.release, nil
}

type holder struct {
	mu       sync.Mutex
	conn     net.Conn
	done     chan struct{}
	released bool
}

func (h *holder) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	close(h.done)
	h.conn.Close()
}

func (h *holder) watch(sock, addr, id, token, desktop string, notify func(error)) {
	for {
		h.mu.Lock()
		c := h.conn
		h.mu.Unlock()
		_, _ = io.Copy(io.Discard, c) // returns when the daemon closes it
		select {
		case <-h.done:
			return
		default:
		}
		notify(errHoldLost)
		err := h.rehold(sock, addr, id, token, desktop)
		select {
		case <-h.done:
			return
		default:
		}
		notify(err)
		if err != nil {
			return
		}
	}
}

// holdGrace is how long a holder waits for someone else (the 2o that
// stopped the daemon to replace it) to start the daemon before starting one
// itself. Without it, an open session running an older 2o would win the race
// and restart the very build that was just replaced.
var holdGrace = 5 * time.Second

func (h *holder) rehold(held, addr, id, token, desktop string) error {
	start := time.Now()
	deadline := start.Add(30 * time.Second)
	wait := 100 * time.Millisecond
	for {
		var sock string
		var err error
		if time.Since(start) < holdGrace {
			if sock = held; !daemonAlive(sock) {
				err = errHoldLost
			}
		} else {
			sock, err = restartDaemon()
		}
		if err == nil {
			var master, newAddr string
			if master, newAddr, err = readKey(keyPath(sock)); err == nil {
				if newAddr != addr {
					return fmt.Errorf("%w: daemon now listens on %s, the tunnel targets %s", errHoldFailed, newAddr, addr)
				}
				var c net.Conn
				if c, err = dialHold(sock, master, id, token, desktop); err == nil {
					h.mu.Lock()
					defer h.mu.Unlock()
					if h.released {
						c.Close()
						return nil
					}
					h.conn = c
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %w", errHoldFailed, err)
		}
		select {
		case <-h.done:
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}

// dialHold opens a hold control connection; it never blocks past its deadlines.
func dialHold(sock, master, id, token, desktop string) (net.Conn, error) {
	c, err := net.DialTimeout("unix", sock, dialTimeout)
	if err != nil {
		return nil, err
	}
	err = c.SetDeadline(time.Now().Add(handshakeTimeout))
	if err == nil {
		err = wire.Write(c, wire.Message{Op: "hold", Name: id, Hash: token, Token: master, Desktop: desktop})
	}
	var reply wire.Message
	if err == nil {
		reply, err = wire.Read(c)
	}
	if err == nil && reply.Error != "" {
		err = errors.New(reply.Error)
	}
	if err == nil {
		err = c.SetDeadline(time.Time{})
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// readKey returns the daemon's master secret and its loopback TCP address.
func readKey(path string) (master, addr string, err error) {
	b, err := readPrivate(path)
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return "", "", fmt.Errorf("malformed daemon key %s", path)
	}
	return f[0], f[1], nil
}

func cacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "2o")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "2o")
}
