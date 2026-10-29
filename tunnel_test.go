package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fastTunnel makes every connection drop at once and retries quickly.
func fastTunnel(t *testing.T) {
	t.Helper()
	oldHold, oldFirst, oldMax := tunnelHold, tunnelRetryFirst, tunnelRetryMax
	tunnelHold, tunnelRetryFirst, tunnelRetryMax = "exit 0", 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { tunnelHold, tunnelRetryFirst, tunnelRetryMax = oldHold, oldFirst, oldMax })
}

// masters counts connection masters started so far (none before the first).
func (f *fakeServer) masters() int {
	b, _ := os.ReadFile(f.log)
	return strings.Count("\n"+string(b), "\n-M\n")
}

func TestTunnelReconnectsAfterDropsAndCleansUp(t *testing.T) {
	f := newFakeServer(t)
	captureUI(t)
	fastTunnel(t)
	o, err := parseSSH([]string{"host"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runTunnel(ctx, o) }()
	deadline := time.Now().Add(10 * time.Second)
	for f.masters() < 3 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no reconnection: %d connections in 10s", f.masters())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tunnelRunning("host") != true {
		t.Fatal("a running tunnel must hold its lock")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopping the tunnel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not stop after cancel")
	}
	if tunnelRunning("host") {
		t.Fatal("lock still held after the tunnel stopped")
	}
	if left, _ := filepath.Glob(filepath.Join(f.tmp, "*")); len(left) != 0 {
		t.Fatalf("session directories left on the server: %q", left)
	}
}

func TestTunnelFirstFailureIsNotRetried(t *testing.T) {
	f := newFakeServer(t)
	captureUI(t)
	fastTunnel(t)
	t.Setenv("FAKE_MASTER_EXIT", "255")
	o, _ := parseSSH([]string{"host"})
	err := runTunnel(context.Background(), o)
	var ue *userError
	if !errors.As(err, &ue) || exitCode(err) != exitSSH {
		t.Fatalf("got %#v, want the ssh connection error", err)
	}
	if n := f.masters(); n != 1 {
		t.Fatalf("a failing first connection was tried %d times", n)
	}
}

func TestTunnelOnePerHost(t *testing.T) {
	newFakeServer(t)
	captureUI(t)
	lock, err := lockTunnel("host")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	o, _ := parseSSH([]string{"host"})
	err = runTunnel(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second tunnel to the same host: %v", err)
	}
	if _, err := lockTunnel("other"); err != nil {
		t.Fatalf("a tunnel to another host must be allowed: %v", err)
	}
}

func TestTunnelRejectsCommandAndConflicts(t *testing.T) {
	for _, args := range []string{"host ls", "-d --stop host", "--stop -d host", "", "-N host"} {
		if err := tunnelCommand(strings.Fields(args)); exitCode(err) != exitUsage {
			t.Errorf("%q: got %v, want a usage error", args, err)
		}
	}
}
