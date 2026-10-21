package main

import (
	"bytes"
	"encoding/binary"
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
