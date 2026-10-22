package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

func TestCompareBuilds(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	self := buildInfo{version: "v2", protocol: protocol, built: t0}
	for name, tc := range map[string]struct {
		peer buildInfo
		want drift
	}{
		"identical":             {buildInfo{"v2", protocol, t0}, sameBuild},
		"older build":           {buildInfo{"v1", protocol, t0.Add(-time.Hour)}, peerOlder},
		"newer build":           {buildInfo{"v3", protocol, t0.Add(time.Hour)}, peerNewer},
		"other protocol":        {buildInfo{"v9", protocol + 1, t0.Add(time.Hour)}, incompatible},
		"pre version-info 2o":   {buildInfo{}, peerUnknown},
		"same name, other time": {buildInfo{"dev", protocol, t0.Add(-time.Minute)}, peerOlder},
		"no build time":         {buildInfo{"dev", protocol, time.Time{}}, peerUnknown},
	} {
		if got := compareBuilds(tc.peer, self); got != tc.want {
			t.Errorf("%s: compareBuilds = %d, want %d", name, got, tc.want)
		}
	}
	if b := parseBuildInfo(versionInfoLine()); b.version != version || b.protocol != protocol {
		t.Fatalf("version-info does not round-trip: %+v", b)
	}
	for _, junk := range []string{"", "2o: unknown command \"version-info\"", "v1 x -"} {
		if b := parseBuildInfo(junk); b.protocol != 0 {
			t.Errorf("%q parsed as %+v, want unknown", junk, b)
		}
	}
}

// Releases compare by version: one release built at different times by
// different packagers is the same 2o, and v0.10.0 is newer than v0.9.0.
func TestCompareReleases(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	self := buildInfo{version: "v0.10.0", protocol: protocol, built: t0}
	for name, tc := range map[string]struct {
		peer buildInfo
		want drift
	}{
		"same release, built later":    {buildInfo{"v0.10.0", protocol, t0.Add(time.Hour)}, sameBuild},
		"older release, built later":   {buildInfo{"v0.9.0", protocol, t0.Add(time.Hour)}, peerOlder},
		"newer release, built before":  {buildInfo{"v0.10.1", protocol, t0.Add(-time.Hour)}, peerNewer},
		"git build, built before":      {buildInfo{"v0.10.0-3-gabc1234", protocol, t0.Add(-time.Hour)}, peerOlder},
		"same release, other protocol": {buildInfo{"v0.10.0", protocol + 1, t0}, incompatible},
	} {
		if got := compareBuilds(tc.peer, self); got != tc.want {
			t.Errorf("%s: compareBuilds = %d, want %d", name, got, tc.want)
		}
	}
	for _, v := range []string{"v1.2", "1.2.3.4", "v1.02.3", "r15.506d1d1", "v1.2.3-rc1", "dev"} {
		if _, ok := releaseVersion(v); ok {
			t.Errorf("%q parsed as a release", v)
		}
	}
}

// answer makes confirmUpdate read s from a fake terminal.
func answer(t *testing.T, s string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	old := askTTY
	askTTY = func() (*os.File, error) { return os.Open(p) }
	t.Cleanup(func() { askTTY = old })
}

func TestConfirmUpdateAnswersAndPolicy(t *testing.T) {
	t.Setenv("TWO_O_UPDATE", "")
	captureUI(t)
	interactive(t, true)
	for in, want := range map[string]bool{"\n": true, "y\n": true, "Y\n": true, "oui\n": true, "n\n": false, "no\n": false, "nope\n": false} {
		answer(t, in)
		if got := confirmUpdate("update?", true); got != want {
			t.Errorf("answer %q with default yes: got %v", in, got)
		}
	}
	answer(t, "\n")
	if confirmUpdate("downgrade?", false) {
		t.Error("empty answer must take the default no")
	}

	answer(t, "n\n")
	t.Setenv("TWO_O_UPDATE", "always")
	if !confirmUpdate("update?", true) {
		t.Error("TWO_O_UPDATE=always must not ask")
	}
	answer(t, "y\n")
	t.Setenv("TWO_O_UPDATE", "never")
	if confirmUpdate("update?", true) {
		t.Error("TWO_O_UPDATE=never must not ask")
	}

	// No terminal: never block, take the default and say so.
	t.Setenv("TWO_O_UPDATE", "")
	interactive(t, false)
	out := captureUI(t)
	if !confirmUpdate("update?", true) || confirmUpdate("downgrade?", false) {
		t.Error("without a terminal the default must apply")
	}
	if !strings.Contains(out.String(), "no terminal to ask") {
		t.Errorf("the decision must be reported: %q", out)
	}
}

func interactive(t *testing.T, on bool) {
	t.Helper()
	old := isInteractive
	isInteractive = func() bool { return on }
	t.Cleanup(func() { isInteractive = old })
}

// The fake server reports an installed 2o; the session must ask before
// replacing it, and keep it when the answer is no.
func TestSSHAsksBeforeUpdatingServer(t *testing.T) {
	t.Setenv("TWO_O_UPDATE", "")
	interactive(t, true)
	f := newFakeServer(t)
	installed := filepath.Join(f.home, ".local", "bin", "2o")
	oldBuild := "#!/bin/sh\ncase \"$1\" in version-info) echo v0 " + strconv.Itoa(protocol) + " 2020-01-01T00:00:00Z;; status) exit 0;; shell) exit 0;; esac\nexit 1\n"
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}

	writeExec(t, installed, oldBuild)
	out := captureUI(t)
	answer(t, "n\n")
	if err := sshSession([]string{"myhost", "true"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(installed); string(b) != oldBuild {
		t.Fatal("answered n, but the server binary was replaced")
	}
	for _, want := range []string{"older 2o detected on myhost: v0", "update 2o on myhost to " + version + "? [Y/n]", "keeping the current 2o on myhost"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}

	out.Reset()
	answer(t, "\n")
	if err := sshSession([]string{"myhost", "true"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(installed); string(b) != fake2o {
		t.Fatal("answered yes (Enter), but the server binary was not updated")
	}
	if !strings.Contains(out.String(), "✓ updating 2o on myhost: v0 → "+version) {
		t.Fatalf("update not reported:\n%s", out)
	}
}

// A server 2o speaking another protocol cannot work: declining stops.
func TestSSHRequiresUpdateForIncompatibleServer(t *testing.T) {
	t.Setenv("TWO_O_UPDATE", "")
	interactive(t, true)
	f := newFakeServer(t)
	installed := filepath.Join(f.home, ".local", "bin", "2o")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	writeExec(t, installed, "#!/bin/sh\n[ \"$1\" = version-info ] && echo v9 "+strconv.Itoa(protocol+1)+" 2030-01-01T00:00:00Z\nexit 1\n")
	captureUI(t)
	answer(t, "n\n")
	err := sshSession([]string{"myhost", "true"})
	if err == nil || exitCode(err) != exitUsage || !strings.Contains(err.Error(), "not updated") {
		t.Fatalf("declining a required update: got %v", err)
	}
	if f.sessionRuns(t) != 0 {
		t.Fatal("the session ran with an incompatible server 2o")
	}
}

// inProcessDaemon makes restartDaemon start the daemon in this process
// instead of exec'ing the test binary as a detached `daemon`, which would
// outlive the test. Like ensureDaemon, a concurrent caller (a session
// re-holding) that finds a daemon already started just uses it.
func inProcessDaemon(t *testing.T, sock string) chan *desktopDaemon {
	t.Helper()
	started := make(chan *desktopDaemon, 4)
	var mu sync.Mutex
	var running []*desktopDaemon
	var wg sync.WaitGroup
	old := restartDaemon
	restartDaemon = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if daemonAlive(sock) {
			return sock, nil
		}
		d, err := openDaemon(sock)
		if err != nil {
			return "", err
		}
		running = append(running, d)
		wg.Add(1)
		go func() { defer wg.Done(); _ = d.serve() }()
		started <- d
		return sock, nil
	}
	t.Cleanup(func() {
		restartDaemon = old
		mu.Lock()
		for _, d := range running {
			d.close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return started
}

// An older desktop daemon is reported and replaced only on "yes"; a session
// held on it re-attaches to the new daemon on the same port.
func TestOlderDaemonIsReplacedAndSessionsReattach(t *testing.T) {
	t.Setenv("TWO_O_UPDATE", "")
	interactive(t, true)
	t.Setenv("XDG_RUNTIME_DIR", daemonDir(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sock, err := socketPath()
	if err != nil {
		t.Fatal(err)
	}
	started := inProcessDaemon(t, sock)
	d, stopped := runDaemon(t, sock)
	d.build = "v0 " + strconv.Itoa(protocol) + " 2020-01-01T00:00:00Z" // the running daemon is "old"
	oldBuilt := buildTime
	buildTime = "2026-10-22T07:00:00Z"
	t.Cleanup(func() { buildTime = oldBuilt })

	master, addr, err := readKey(keyPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	s := session{addr: addr}
	s.id, _ = randomHex(12)
	s.token, _ = randomHex(32)
	events := make(chan error, 4)
	release, err := holdSession(sock, master, s.id, s.token, "me@desk", func(e error) { events <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// "n": reported, nothing changes.
	out := captureUI(t)
	answer(t, "n\n")
	if _, err := startDaemon(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"older desktop daemon detected: v0", "1 open session will reconnect", "keeping the running desktop daemon v0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	select {
	case <-stopped:
		t.Fatal("answered n, but the daemon stopped")
	default:
	}

	// "y": the old daemon exits, this build starts on the same port, and
	// the held session is restored there.
	out.Reset()
	answer(t, "y\n")
	if _, err := startDaemon(); err != nil {
		t.Fatal(err)
	}
	<-stopped
	if !strings.Contains(out.String(), "✓ restarting desktop daemon with 2o "+version) {
		t.Fatalf("restart not reported:\n%s", out)
	}
	if info, _, err := daemonInfo(sock); err != nil || info.version != version {
		t.Fatalf("new daemon reports %+v, %v", info, err)
	}
	if _, a2, _ := readKey(keyPath(sock)); a2 != addr {
		t.Fatalf("the new daemon moved from %s to %s: SSH forwards would break", addr, a2)
	}
	if e := <-events; !errors.Is(e, errHoldLost) {
		t.Fatalf("first event %v, want errHoldLost", e)
	}
	if e := <-events; e != nil {
		t.Fatalf("session was not re-held on the new daemon: %v", e)
	}
	c, err := dialSession(s, wire.Message{Op: "ping"})
	if err != nil {
		t.Fatalf("session does not work on the new daemon: %v", err)
	}
	c.Close()
	if len(started) != 1 {
		t.Fatalf("started %d daemons, want 1", len(started))
	}
}
