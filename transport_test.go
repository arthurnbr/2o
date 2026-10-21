package main

import (
	"bytes"
	"encoding/binary"
	"net"
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

func TestSSHStartedInsideSSHRejectsWrongConnectionDirection(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "100.71.225.71 50000 100.93.47.7 22")
	err := sshSession([]string{"omarchy"})
	if err == nil || !strings.Contains(err.Error(), "run on the desktop") && !strings.Contains(err.Error(), "must run on the desktop") {
		t.Fatalf("nested SSH should explain which end must start 2o: %v", err)
	}
	if !strings.Contains(err.Error(), "existing ssh session") {
		t.Fatalf("existing SSH connection cannot be retrofitted: %v", err)
	}
}
func TestSessionHandshakeChecksExactForwardedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			serve(conn)
		}
	}()
	if err := checkSocket(path); err != nil {
		t.Fatalf("live desktop handshake failed: %v", err)
	}
	<-done
	if err := checkSocket(filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("accepted an absent forwarded socket")
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
