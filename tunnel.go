package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/ui"
)

// `2o tunnel HOST` keeps a desktop tunnel to HOST open without a shell, and
// reconnects whenever the connection drops. Shells on HOST that have no
// session of their own (multiplexer panes, plain ssh logins) use it through
// the latest-live-session fallback. With -d it runs in the background.

// Reconnection delays: doubling from the first to the last, back to the
// first once a connection held for tunnelStable. Vars for tests.
var (
	tunnelRetryFirst = time.Second
	tunnelRetryMax   = time.Minute
	tunnelStable     = time.Minute
)

// tunnelKeepalive makes the connection master notice a dead network within
// about 45s, so the tunnel reconnects. The user's own -o values come first
// and win; these override ~/.ssh/config.
var tunnelKeepalive = []string{"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}

// tunnelHold is the remote command that keeps the connection busy; the
// tunnel itself lives in the connection master. A var for tests.
var tunnelHold = "exec sleep 2147483647"

// tunnelReadyEnv names the file descriptor a detached tunnel reports its
// first connection on: "ok", or the error line.
const tunnelReadyEnv = "TWO_O_TUNNEL_READY"

type tunnelArgs struct {
	detach, stop bool
	ssh          []string
}

func parseTunnel(args []string) (tunnelArgs, error) {
	var a tunnelArgs
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "-d", "--detach":
			a.detach = true
			continue
		case "--stop":
			a.stop = true
			continue
		}
		break
	}
	a.ssh = args[i:]
	if a.stop && a.detach {
		return a, usageError("tunnel", "--stop and --detach cannot be combined")
	}
	return a, nil
}

func tunnelCommand(args []string) error {
	a, err := parseTunnel(args)
	if err != nil {
		return err
	}
	o, err := parseSSH(a.ssh)
	if err != nil {
		return err
	}
	if len(o.command) != 0 {
		return usageError("tunnel", "2o tunnel runs no command on %s", o.host)
	}
	if a.stop {
		return stopTunnel(o.host)
	}
	if a.detach {
		return detachTunnel(o.host, a.ssh)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return runTunnel(ctx, o)
}

// tunnelFile is the per-host lock and pid file of a running tunnel.
func tunnelFile(host string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@-", r) {
			return r
		}
		return '_'
	}, host)
	return filepath.Join(cacheDir(), "tunnel-"+safe+".pid")
}

func tunnelLog(host string) string { return strings.TrimSuffix(tunnelFile(host), ".pid") + ".log" }

// lockTunnel takes HOST's tunnel lock for this process and records its pid.
// The lock is released when the process exits, however it exits.
func lockTunnel(host string) (*os.File, error) {
	if err := os.MkdirAll(cacheDir(), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(tunnelFile(host), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		pid := tunnelPid(host)
		return nil, fail("a tunnel to %s is already running (pid %d)", host, pid).
			hint("stop it with: 2o tunnel --stop %s", host)
	}
	if err = f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func tunnelPid(host string) int {
	b, _ := os.ReadFile(tunnelFile(host))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// tunnelRunning reports whether some process holds HOST's tunnel lock.
func tunnelRunning(host string) bool {
	f, err := os.Open(tunnelFile(host))
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func stopTunnel(host string) error {
	if !tunnelRunning(host) {
		return fail("no tunnel to %s is running", host).exit(exitNoTunnel)
	}
	pid := tunnelPid(host)
	if pid <= 0 {
		return fail("cannot find the tunnel's process in %s", tunnelFile(host))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fail("cannot stop the tunnel to %s (pid %d)", host, pid).because(err)
	}
	for range 100 {
		if !tunnelRunning(host) {
			ui.Success("tunnel to %s stopped", host)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fail("the tunnel to %s (pid %d) did not stop within 10s", host, pid)
}

// detachTunnel starts `2o tunnel` in its own session, logging to a file, and
// returns once its first connection is up (or has failed). The background
// tunnel cannot prompt: ssh runs in batch mode, so keys come from ssh-agent
// or unencrypted files, and the host key must already be known.
func detachTunnel(host string, sshArgs []string) error {
	if tunnelRunning(host) {
		return fail("a tunnel to %s is already running (pid %d)", host, tunnelPid(host)).
			hint("stop it with: 2o tunnel --stop %s", host)
	}
	self, err := selfExecutable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(cacheDir(), 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(tunnelLog(host), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logf.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd := exec.Command(self, append([]string{"tunnel"}, sshArgs...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{w} // fd 3
	cmd.Env = append(os.Environ(), tunnelReadyEnv+"=3", "TWO_O_QUIET=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	step := ui.Start("starting a background tunnel to %s", host)
	if err = cmd.Start(); err != nil {
		w.Close()
		step.Fail()
		return fail("cannot start the background tunnel").because(err)
	}
	w.Close()
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "ok" {
		step.Fail()
		if line == "" {
			line = "the background tunnel exited"
		}
		return fail("%s", line).hint("see its log: %s", tunnelLog(host))
	}
	step.Done()
	ui.Success("tunnel to %s runs in the background (pid %d)", host, pid)
	ui.Hint("log: %s — stop: 2o tunnel --stop %s", tunnelLog(host), host)
	return nil
}

// readyReporter returns where a detached tunnel reports its first outcome,
// or nil in the foreground.
func readyReporter() io.WriteCloser {
	fd, err := strconv.Atoi(os.Getenv(tunnelReadyEnv))
	os.Unsetenv(tunnelReadyEnv)
	if err != nil || fd < 3 {
		return nil
	}
	return os.NewFile(uintptr(fd), "ready")
}

// runTunnel connects, then reconnects after every drop, until ctx ends. Only
// a first connection that fails is an error: it is a configuration problem
// (host, key, server) that retrying would hide.
func runTunnel(ctx context.Context, o sshOptions) error {
	if o.quiet {
		ui.SetQuiet(true)
	}
	ready := readyReporter()
	report := func(s string) {
		if ready != nil {
			fmt.Fprintln(ready, s)
			ready.Close()
			ready = nil
		}
	}
	defer report("the background tunnel exited")
	if ready != nil {
		o.master = append(o.master, "-o", "BatchMode=yes")
	}
	o.master = append(o.master, tunnelKeepalive...)

	lock, err := lockTunnel(o.host)
	if err != nil {
		report(errorLine(err))
		return err
	}
	defer lock.Close()

	wait := tunnelRetryFirst
	var stale []string // session directories a dropped connection left on the server
	for first := true; ; first = false {
		ui.Header("2o tunnel → %s", o.host)
		up, err := holdTunnel(ctx, o, &stale, func() {
			if first {
				report("ok")
			}
		})
		if ctx.Err() != nil {
			ui.Note("tunnel closed")
			return nil
		}
		if first && up == 0 {
			// Never connected: a configuration problem the user must see.
			report(errorLine(err))
			return err
		}
		if up >= tunnelStable {
			wait = tunnelRetryFirst
		}
		ui.Warn("tunnel to %s lost: %s", o.host, errorLine(err))
		ui.Note("reconnecting in %s", wait)
		select {
		case <-ctx.Done():
			ui.Note("tunnel closed")
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, tunnelRetryMax)
	}
}

// errTunnelDropped ends a connection that was established.
var errTunnelDropped = errors.New("the connection dropped")

// holdTunnel opens one tunnel and keeps it until the connection drops, the
// desktop session cannot be restored, or ctx ends. It returns how long the
// tunnel was up (0 if it never was) and why it ended. Once connected, it
// removes the stale directories, and adds its own if it cannot remove it.
func holdTunnel(ctx context.Context, o sshOptions, stale *[]string, onUp func()) (time.Duration, error) {
	conn, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	notify := func(err error) {
		switch {
		case err == nil:
			ui.Info("desktop daemon is back, tunnel restored")
		case errors.Is(err, errHoldFailed):
			cancel(err)
		default:
			ui.Warn("desktop daemon stopped, waiting for it to come back")
		}
	}
	t, err := openDesktopTunnel(conn, o, notify)
	if err != nil {
		return 0, err
	}
	defer func() {
		if !t.close() {
			*stale = append(*stale, t.dir)
		}
	}()
	start := time.Now()
	ui.Success("desktop tunnel to %s ready — files, links and clipboard open on %s", o.host, t.desktop)
	onUp()
	if len(*stale) > 0 {
		q := make([]string, len(*stale))
		for i, d := range *stale {
			q[i] = quote(d)
		}
		if _, err := t.c.run("rm -rf -- "+strings.Join(q, " "), nil); err == nil {
			*stale = nil
		}
	}

	t.c.ctx = conn
	hold := t.c.client("-T", o.host, tunnelHold)
	// No pipes: the client hands its descriptors to the connection master,
	// which would keep a pipe open and Wait would never return.
	hold.Stdin, hold.Stdout, hold.Stderr = nil, nil, nil
	hold.WaitDelay = 5 * time.Second
	_ = hold.Run()
	err = errTunnelDropped
	if cause := context.Cause(conn); errors.Is(cause, errHoldFailed) {
		err = cause
	}
	return time.Since(start), err
}
