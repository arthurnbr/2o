package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurnbr/2o/internal/wire"
)

func TestSSHOptionsAndCommand(t *testing.T) {
	opts, host, command, err := splitSSH([]string{"-J", "bastion", "-p", "222", "-o", "ProxyCommand=none", "user@tailnet", "2o", "notify", "done", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "user@tailnet" || strings.Join(opts, "|") != "-J|bastion|-p|222|-o|ProxyCommand=none" || strings.Join(command, "|") != "2o|notify|done|ok" {
		t.Fatalf("options %q host %q command %q", opts, host, command)
	}
	if _, _, _, err = splitSSH([]string{"-J"}); err == nil {
		t.Fatal("accepted missing jump host")
	}
}

// startDesktop runs the daemon's authenticate+serve loop on loopback TCP and
// returns a private session file for a registered session.
func startDesktop(t *testing.T) (sessionFile string, reg *registry, addr string) {
	t.Helper()
	master, _ := randomHex(32)
	reg = newRegistry(master)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				m, err := reg.authenticate(c)
				if err != nil {
					_ = wire.Write(c, wire.Message{Error: err.Error()})
					return
				}
				serve(c, m)
			}()
		}
	}()
	id, _ := randomHex(12)
	token, _ := randomHex(32)
	reg.mu.Lock()
	reg.tokens[id] = token
	reg.mu.Unlock()
	dir := filepath.Join(t.TempDir(), "s")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sessionFile = filepath.Join(dir, "session")
	if err = os.WriteFile(sessionFile, []byte(l.Addr().String()+" "+id+" "+token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWO_O_SESSION", sessionFile)
	return sessionFile, reg, l.Addr().String()
}

func TestAuthenticatedSessionPing(t *testing.T) {
	startDesktop(t)
	c, err := request(wire.Message{Op: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = response(c); err != nil {
		t.Fatal(err)
	}
}

func TestWrongTokenIsRejectedBeforeAnyRequestIsServed(t *testing.T) {
	file, _, addr := startDesktop(t)
	s, err := readSession(file)
	if err != nil {
		t.Fatal(err)
	}
	bad := s
	bad.token = strings.Repeat("0", 64)
	if _, err = dialSession(bad, wire.Message{Op: "ping"}); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("client must refuse a daemon that cannot prove the token: %v", err)
	}
	// A peer with the session id but no token cannot get a request served.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	nonce := strings.Repeat("a", 32)
	if err = wire.Write(c, wire.Message{Op: "hello", Name: s.id, Hash: nonce}); err != nil {
		t.Fatal(err)
	}
	if _, err = wire.Read(c); err != nil {
		t.Fatal(err)
	}
	if err = wire.Write(c, wire.Message{Op: "ping", Token: "forged"}); err != nil {
		t.Fatal(err)
	}
	reply, err := wire.Read(c)
	if err != nil || reply.Error != "unauthorized" {
		t.Fatalf("forged token reply %+v err %v", reply, err)
	}
}

func TestUnregisteredSessionAndImpostorAreRefused(t *testing.T) {
	file, reg, _ := startDesktop(t)
	s, _ := readSession(file)
	reg.mu.Lock()
	delete(reg.tokens, s.id)
	reg.mu.Unlock()
	if _, err := request(wire.Message{Op: "ping"}); err == nil {
		t.Fatal("closed session still accepted")
	}
	// An impostor listening on the forwarded port must not receive the request.
	imp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer imp.Close()
	got := make(chan wire.Message, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := imp.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		m, _ := wire.Read(c)
		got <- m
		_ = wire.Write(c, wire.Message{Hash: strings.Repeat("f", 64)})
		if m, err = wire.Read(c); err == nil {
			got <- m
		}
	}()
	s.addr = imp.Addr().String()
	if _, err = dialSession(s, wire.Message{Op: "open-file", Name: "secret.pdf"}); err == nil {
		t.Fatal("client sent to an unauthenticated endpoint")
	}
	<-done
	if first := <-got; first.Op != "hello" {
		t.Fatalf("unexpected first message %+v", first)
	}
	select {
	case m := <-got:
		t.Fatalf("impostor received request %+v", m)
	default:
	}
}

func TestSessionFileMustBePrivate(t *testing.T) {
	file, _, _ := startDesktop(t)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(file); err == nil {
		t.Fatal("accepted a world-readable session file")
	}
}

func TestFramePreservesBinaryPayloadAndRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.Write(&buf, wire.Message{Op: "open-file", Name: "a.pdf", Size: 4}); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 255, 1, 10}
	buf.Write(payload)
	m, err := wire.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if m.Op != "open-file" || m.Size != 4 || m.Name != "a.pdf" {
		t.Fatalf("header: %+v", m)
	}
	b := make([]byte, 4)
	if _, err = buf.Read(b); err != nil || !bytes.Equal(b, payload) {
		t.Fatalf("payload %x: %v", b, err)
	}
	var bad [4]byte
	binary.BigEndian.PutUint32(bad[:], wire.MaxHeader+1)
	if _, err = wire.Read(bytes.NewReader(bad[:])); err == nil {
		t.Fatal("accepted oversized header")
	}
}
