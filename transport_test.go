package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

type testDesktop struct {
	sock    string        // control socket
	addr    string        // daemon loopback TCP address
	file    string        // private session file of the held session
	s       session       // that session
	release func()        // revokes the hold
	stop    func()        // stops the daemon and waits for its loop to return
	stopped chan struct{} // closed when the daemon loop returned
}

// daemonDir is a private directory with a short path (Unix socket limit).
func daemonDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "2o-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// runDaemon runs the production daemon loop on the control socket sock.
func runDaemon(t *testing.T, sock string) (d *desktopDaemon, stopped chan struct{}) {
	t.Helper()
	d, err := openDaemon(sock)
	if err != nil {
		t.Fatal(err)
	}
	stopped = make(chan struct{})
	go func() { defer close(stopped); _ = d.serve() }()
	t.Cleanup(func() { d.close(); <-stopped })
	return d, stopped
}

// holdTestSession holds a fresh session on the daemon at sock and writes its
// private session file under dir.
func holdTestSession(t *testing.T, sock, dir, desktop string) (string, session, func()) {
	t.Helper()
	master, addr, err := readKey(keyPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := randomHex(12)
	token, _ := randomHex(32)
	release, err := holdSession(sock, master, id, token, desktop, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "session")
	if err = os.WriteFile(file, []byte(addr+" "+id+" "+token+" "+desktop+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return file, session{path: file, addr: addr, id: id, token: token, desktop: desktop}, release
}

// startDesktop runs a real daemon loop (control socket + loopback TCP) and
// holds a session for this shell, exactly as `2o ssh` does.
func startDesktop(t *testing.T) *testDesktop {
	t.Helper()
	sock := filepath.Join(daemonDir(t), "2o.sock")
	d, stopped := runDaemon(t, sock)
	file, s, release := holdTestSession(t, sock, filepath.Join(t.TempDir(), "s"), "me@desk")
	t.Setenv("TWO_O_SESSION", file)
	t.Setenv("TWO_O_DESKTOP", "")
	return &testDesktop{sock: sock, addr: s.addr, file: file, s: s, release: release,
		stop: func() { d.close(); <-stopped }, stopped: stopped}
}

// handshake runs the client side by hand up to the server's reply.
func handshake(t *testing.T, s session) (c net.Conn, clientNonce, serverNonce string) {
	t.Helper()
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	clientNonce = strings.Repeat("a", nonceLen)
	if err = wire.Write(c, wire.Message{Op: "hello", Name: s.id, Hash: clientNonce}); err != nil {
		t.Fatal(err)
	}
	reply, err := wire.Read(c)
	if err != nil || reply.Error != "" {
		t.Fatalf("hello reply %+v: %v", reply, err)
	}
	return c, clientNonce, reply.Token
}

func roundTrip(t *testing.T, c net.Conn, m wire.Message) wire.Message {
	t.Helper()
	if err := wire.Write(c, m); err != nil {
		t.Fatal(err)
	}
	reply, err := wire.Read(c)
	if err != nil {
		t.Fatal(err)
	}
	return reply
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
	d := startDesktop(t)
	bad := d.s
	bad.token = strings.Repeat("0", 64)
	if _, err := dialSession(bad, wire.Message{Op: "ping"}); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("client must refuse a daemon that cannot prove the token: %v", err)
	}
	// A peer with the session id but no token cannot get a request served.
	c, _, _ := handshake(t, d.s)
	if reply := roundTrip(t, c, wire.Message{Op: "ping", Token: "forged"}); reply.Error != "unauthorized" {
		t.Fatalf("forged proof reply %+v", reply)
	}
}

// A captured exchange (hello + proof-carrying request) is useless on a new
// connection: the daemon picks a fresh nonce, so the old proof does not match.
func TestReplayedHandshakeIsRejected(t *testing.T) {
	d := startDesktop(t)
	c, cn, sn := handshake(t, d.s)
	req := wire.Message{Op: "ping"}
	req.Token = clientProof(d.s.token, d.s.id, cn, sn, req)
	if reply := roundTrip(t, c, req); reply.Error != "" {
		t.Fatalf("genuine request refused: %+v", reply)
	}
	c2, _, sn2 := handshake(t, d.s)
	if sn2 == sn {
		t.Fatal("daemon reused a server nonce")
	}
	if reply := roundTrip(t, c2, req); reply.Error != "unauthorized" {
		t.Fatalf("replayed proof accepted: %+v", reply)
	}
}

// The proof covers the request header: changing any acted-on field after
// computing it (here ping → paste, or a different file name) is refused.
func TestTamperedRequestIsRejected(t *testing.T) {
	d := startDesktop(t)
	for _, tamper := range []func(*wire.Message){
		func(m *wire.Message) { m.Op = "paste" },
		func(m *wire.Message) { m.Name = "other.pdf" },
		func(m *wire.Message) { m.Size++ },
		func(m *wire.Message) { m.URL = "https://evil.example" },
	} {
		c, cn, sn := handshake(t, d.s)
		m := wire.Message{Op: "open-file", Name: "a.pdf", Size: 3}
		m.Token = clientProof(d.s.token, d.s.id, cn, sn, m)
		tamper(&m)
		if reply := roundTrip(t, c, m); reply.Error != "unauthorized" {
			t.Fatalf("tampered request %+v accepted: %+v", m, reply)
		}
	}
}

func TestUnheldSessionAndImpostorAreRefused(t *testing.T) {
	d := startDesktop(t)
	d.release()
	if _, err := request(wire.Message{Op: "ping"}); err == nil {
		t.Fatal("released session still accepted")
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
		_ = wire.Write(c, wire.Message{Hash: strings.Repeat("f", 64), Token: strings.Repeat("b", nonceLen)})
		if m, err = wire.Read(c); err == nil {
			got <- m
		}
	}()
	s := d.s
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

// expectClosed fails unless the daemon closes c promptly.
func expectClosed(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s: connection still open", what)
	}
}

// openTransfer starts a copy whose payload never comes: the daemon waits on it.
func openTransfer(t *testing.T) net.Conn {
	t.Helper()
	c, err := request(wire.Message{Op: "copy", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestReleasingHoldClosesActiveConnections(t *testing.T) {
	d := startDesktop(t)
	c := openTransfer(t)
	d.release()
	expectClosed(t, c, "active transfer after revocation")
	if _, err := request(wire.Message{Op: "ping"}); err == nil {
		t.Fatal("revoked session still accepted")
	}
}

// A handshake that loaded the token before revocation cannot complete.
func TestRevocationStopsHandshakeInFlight(t *testing.T) {
	d := startDesktop(t)
	c, cn, sn := handshake(t, d.s)
	d.release()
	// The daemon processes the release asynchronously; wait until it is effective.
	for i := 0; ; i++ {
		if _, err := dialSession(d.s, wire.Message{Op: "ping"}); err != nil {
			break
		}
		if i == 100 {
			t.Fatal("hold never revoked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	req := wire.Message{Op: "ping"}
	req.Token = clientProof(d.s.token, d.s.id, cn, sn, req)
	if err := wire.Write(c, req); err != nil {
		t.Fatal(err)
	}
	if reply, err := wire.Read(c); err == nil && reply.Error == "" {
		t.Fatalf("request served after revocation: %+v", reply)
	}
}

// TestHelperHolder is the holder process of TestKilledHolderRevokesSession.
func TestHelperHolder(t *testing.T) {
	sock := os.Getenv("TWO_O_TEST_HOLD_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	master, _, err := readKey(keyPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holdSession(sock, master, os.Getenv("TWO_O_TEST_HOLD_ID"), os.Getenv("TWO_O_TEST_HOLD_TOKEN"), "me@desk", nil); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("held\n")
	time.Sleep(time.Minute)
}

// kill -9 of `2o ssh` must not leave a valid secret behind.
func TestKilledHolderRevokesSession(t *testing.T) {
	sock := filepath.Join(daemonDir(t), "2o.sock")
	runDaemon(t, sock)
	_, addr, err := readKey(keyPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	s := session{addr: addr}
	s.id, _ = randomHex(12)
	s.token, _ = randomHex(32)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHolder$")
	cmd.Env = append(os.Environ(), "TWO_O_TEST_HOLD_SOCK="+sock, "TWO_O_TEST_HOLD_ID="+s.id, "TWO_O_TEST_HOLD_TOKEN="+s.token)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line := make([]byte, 5)
	if _, err = io.ReadFull(out, line); err != nil || string(line) != "held\n" {
		t.Fatalf("holder did not start: %q %v", line, err)
	}
	c, err := dialSession(s, wire.Message{Op: "copy", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "connection of a killed holder's session")
	if _, err = dialSession(s, wire.Message{Op: "ping"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("killed holder's session still accepted: %v", err)
	}
}

func TestDaemonKeepsPortAcrossRestarts(t *testing.T) {
	sock := filepath.Join(daemonDir(t), "2o.sock")
	d, err := openDaemon(sock)
	if err != nil {
		t.Fatal(err)
	}
	first := d.tcp.Addr().String()
	d.close()
	d, err = openDaemon(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.tcp.Addr().String(); got != first {
		t.Fatalf("restarted on %s, was %s", got, first)
	}
	if _, addr, err := readKey(keyPath(sock)); err != nil || addr != first {
		t.Fatalf("key file addr %q: %v", addr, err)
	}
	d.close()
	// Port taken meanwhile: a new one is chosen and recorded.
	busy, err := net.Listen("tcp", first)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	d, err = openDaemon(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	moved := d.tcp.Addr().String()
	if moved == first {
		t.Fatal("bound a busy port")
	}
	if b, err := readPrivate(portPath(sock)); err != nil || "127.0.0.1:"+strings.TrimSpace(string(b)) != moved {
		t.Fatalf("port file %q does not record %s: %v", b, moved, err)
	}
}

// A restarted daemon gets the hold back without the holder doing anything,
// and the session's token works again on the same port.
func TestHoldSurvivesDaemonRestart(t *testing.T) {
	sock := filepath.Join(daemonDir(t), "2o.sock")
	d, stopped := runDaemon(t, sock)
	master, addr, err := readKey(keyPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	s := session{addr: addr}
	s.id, _ = randomHex(12)
	s.token, _ = randomHex(32)
	events := make(chan error, 4)
	old := restartDaemon
	restarted := make(chan *desktopDaemon, 1)
	restartDaemon = func() (string, error) {
		d2, err := openDaemon(sock)
		if err != nil {
			return "", err
		}
		go func() { _ = d2.serve() }()
		restarted <- d2
		return sock, nil
	}
	t.Cleanup(func() { restartDaemon = old })
	release, err := holdSession(sock, master, s.id, s.token, "me@desk", func(e error) { events <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	d.close()
	<-stopped
	if e := <-events; !errors.Is(e, errHoldLost) {
		t.Fatalf("first event %v", e)
	}
	if e := <-events; e != nil {
		t.Fatalf("re-hold failed: %v", e)
	}
	defer (<-restarted).close()
	c, err := dialSession(s, wire.Message{Op: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// globRoot points the session-file search at a fresh directory.
func globRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := sessionGlob
	sessionGlob = filepath.Join(root, "2o-%d-*", "session")
	t.Cleanup(func() { sessionGlob = old })
	return root
}

// sessionDir is a /tmp/2o-UID-NAME lookalike under root.
func sessionDir(root, name string) string {
	return filepath.Join(root, fmt.Sprintf("2o-%d-%s", os.Getuid(), name))
}

// A stale TWO_O_SESSION may move on only to the one live session of the same
// desktop; several candidates, or another desktop, are listed instead.
func TestStaleSessionRouting(t *testing.T) {
	d := startDesktop(t)
	root := globRoot(t)
	stale := filepath.Join(sessionDir(root, "gone"), "session")
	t.Setenv("TWO_O_SESSION", stale)
	t.Setenv("TWO_O_DESKTOP", "me@desk")

	// One live session of the same desktop: used.
	holdTestSession(t, d.sock, sessionDir(root, "a"), "me@desk")
	c, err := request(wire.Message{Op: "ping"})
	if err != nil {
		t.Fatalf("same-desktop fallback refused: %v", err)
	}
	c.Close()

	// Two live sessions: ambiguous, both listed with how to pick.
	other, _, _ := holdTestSession(t, d.sock, sessionDir(root, "b"), "you@laptop")
	_, err = request(wire.Message{Op: "ping"})
	var ue *userError
	if !errors.As(err, &ue) || ue.code != exitNoTunnel {
		t.Fatalf("ambiguous routing: %v", err)
	}
	hints := strings.Join(ue.hints, "\n")
	if !strings.Contains(hints, "export TWO_O_SESSION="+other) || !strings.Contains(hints, "you@laptop") || !strings.Contains(hints, "me@desk") {
		t.Fatalf("hints do not list the sessions:\n%s", hints)
	}

	// Only another desktop is live: never silently switched to.
	if err = os.Remove(filepath.Join(sessionDir(root, "a"), "session")); err != nil {
		t.Fatal(err)
	}
	_, err = request(wire.Message{Op: "ping"})
	if !errors.As(err, &ue) || ue.code != exitNoTunnel || !strings.Contains(strings.Join(ue.hints, "\n"), other) {
		t.Fatalf("switched to another desktop or bad error: %v", err)
	}
}

func TestNoSessionIsAUserError(t *testing.T) {
	globRoot(t)
	t.Setenv("TWO_O_SESSION", "")
	_, err := request(wire.Message{Op: "ping"})
	var ue *userError
	if !errors.As(err, &ue) || ue.code != exitNoTunnel || len(ue.hints) == 0 {
		t.Fatalf("got %v", err)
	}
	t.Setenv("TWO_O_SESSION", filepath.Join(t.TempDir(), "missing", "session"))
	_, err = request(wire.Message{Op: "ping"})
	if !errors.As(err, &ue) || ue.msg != "desktop tunnel unavailable" || !strings.Contains(ue.hints[0], "2o ssh HOST") || ue.cause == nil {
		t.Fatalf("stale session error %#v", err)
	}
}

func TestSessionFileMustBePrivate(t *testing.T) {
	d := startDesktop(t)
	if err := os.Chmod(d.file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(d.file); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("accepted a world-readable session file: %v", err)
	}
}

// Symlinks are refused for the file and its directory (O_NOFOLLOW + fstat),
// even when they point at a perfectly private target.
func TestSessionFileSymlinksAreRefused(t *testing.T) {
	d := startDesktop(t)
	base := t.TempDir()
	link := filepath.Join(base, "session")
	if err := os.Symlink(d.file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(link); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("followed a session file symlink: %v", err)
	}
	dirLink := filepath.Join(base, "dir")
	if err := os.Symlink(filepath.Dir(d.file), dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(filepath.Join(dirLink, "session")); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("followed a session directory symlink: %v", err)
	}
	fifo := filepath.Join(filepath.Dir(d.file), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(fifo); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("accepted a FIFO: %v", err)
	}
}

func TestStalledTransferTimesOut(t *testing.T) {
	old := idleTimeout
	idleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { idleTimeout = old })
	startDesktop(t)
	c := openTransfer(t)
	expectClosed(t, c, "stalled transfer")
}

func TestConnectionCap(t *testing.T) {
	old := maxConns
	maxConns = 1
	t.Cleanup(func() { maxConns = old })
	startDesktop(t)
	openTransfer(t)
	_, err := request(wire.Message{Op: "ping"})
	var ue *userError
	if !errors.As(err, &ue) || ue.msg != "desktop daemon is busy" || ue.code != exitNoTunnel {
		t.Fatalf("over the cap: %v", err)
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
