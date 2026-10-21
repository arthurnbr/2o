package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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
// flows: the daemon proves it knows the secret first, so an impostor bound to a
// stale port never receives file contents.

const sessionGlob = "/tmp/2o-%d-*/session"

type session struct {
	addr  string
	id    string
	token string
}

type registry struct {
	master string
	mu     sync.Mutex
	tokens map[string]string
}

func newRegistry(master string) *registry {
	return &registry{master: master, tokens: make(map[string]string)}
}

func (r *registry) lookup(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[id]
	return t, ok
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func proof(token, role, nonce string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(role))
	mac.Write([]byte{0})
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// privateFile rejects files that are not regular, not owned by the caller or
// readable by others, and directories other users could write into.
func privateFile(path string) error {
	for _, p := range []string{filepath.Dir(path), path} {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("%s is not private to this user", p)
		}
		if p == path && !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
	}
	return nil
}

func readSession(path string) (session, error) {
	if err := privateFile(path); err != nil {
		return session{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return session{}, err
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		return session{}, fmt.Errorf("malformed session file %s", path)
	}
	return session{addr: f[0], id: f[1], token: f[2]}, nil
}

// dialSession authenticates both ends, then sends m as the first request.
func dialSession(s session, m wire.Message) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", s.addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if err = c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fail(err)
	}
	nonce, err := randomHex(16)
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
		return fail(errors.New(reply.Error))
	}
	if !equal(reply.Hash, proof(s.token, "server", nonce)) {
		return fail(errors.New("endpoint failed authentication; refusing to send data"))
	}
	m.Token = proof(s.token, "client", nonce)
	if err = wire.Write(c, m); err != nil {
		return fail(err)
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return c, nil
}

// request dials the current session, falling back to the newest live session
// of this user when the inherited one is gone (e.g. a reattached tmux).
func request(m wire.Message) (net.Conn, error) {
	var firstErr error
	candidates := []string{}
	if p := os.Getenv("TWO_O_SESSION"); p != "" {
		candidates = append(candidates, p)
	}
	matches, _ := filepath.Glob(fmt.Sprintf(sessionGlob, os.Getuid()))
	sort.Slice(matches, func(i, j int) bool { return modTime(matches[i]).After(modTime(matches[j])) })
	for _, p := range matches {
		if p != os.Getenv("TWO_O_SESSION") {
			candidates = append(candidates, p)
		}
	}
	for _, p := range candidates {
		s, err := readSession(p)
		if err == nil {
			var c net.Conn
			if c, err = dialSession(s, m); err == nil {
				return c, nil
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", p, err)
		}
	}
	if firstErr != nil {
		return nil, fmt.Errorf("no reachable 2o desktop (%v); reconnect with '2o ssh HOST'", firstErr)
	}
	return nil, errors.New("no 2o session here; connect with '2o ssh HOST' from the machine where things should open")
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// authenticate runs the daemon side of the handshake and returns the actual
// request. Register/unregister requests carry the master secret, which never
// leaves the desktop.
func (r *registry) authenticate(c net.Conn) (wire.Message, error) {
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return wire.Message{}, err
	}
	m, err := wire.Read(c)
	if err != nil {
		return m, err
	}
	switch m.Op {
	case "register", "unregister":
		if !equal(m.Token, r.master) {
			return m, errors.New("unauthorized")
		}
		r.mu.Lock()
		if m.Op == "register" {
			r.tokens[m.Name] = m.Hash
		} else {
			delete(r.tokens, m.Name)
		}
		r.mu.Unlock()
		return m, wire.Write(c, wire.Message{})
	case "hello":
		token, ok := r.lookup(m.Name)
		if !ok || len(m.Hash) != 32 {
			return m, errors.New("unknown or closed 2o session")
		}
		nonce := m.Hash
		if err = wire.Write(c, wire.Message{Hash: proof(token, "server", nonce)}); err != nil {
			return m, err
		}
		req, err := wire.Read(c)
		if err != nil {
			return req, err
		}
		if !equal(req.Token, proof(token, "client", nonce)) {
			return req, errors.New("unauthorized")
		}
		return req, c.SetDeadline(time.Time{})
	default:
		return m, errors.New("unauthorized")
	}
}

// controlRequest sends a master-authenticated register/unregister to the local daemon.
func controlRequest(sock, master string, m wire.Message) error {
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	m.Token = master
	if err = wire.Write(c, m); err != nil {
		return err
	}
	return response(c)
}

// readKey returns the daemon's master secret and its loopback TCP address.
func readKey(path string) (master, addr string, err error) {
	if err = privateFile(path); err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return "", "", fmt.Errorf("malformed daemon key %s", path)
	}
	return f[0], f[1], nil
}

// freePort returns a currently unused loopback TCP port on this host.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func cacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "2o")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "2o")
}
