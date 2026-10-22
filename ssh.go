package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/ui"
	"github.com/arthurnbr/2o/internal/wire"
)

// `2o ssh HOST` opens one private OpenSSH connection master (one
// authentication), prepares the server through it, adds the remote forward to
// the desktop daemon, verifies the tunnel, and only then runs the user's shell
// or command exactly once. The command is never re-run: its exit code, or
// ssh's own 255, is returned as is.

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// sshOptions is a parsed `2o ssh` command line.
type sshOptions struct {
	host    string
	command []string
	master  []string // connection options: given to the connection master only
	session []string // per-session options: given to the user's ssh only
	tty     string   // "-t", "-tt"… or "-T" when the user chose; "" = default
	ttyConf bool     // the user set -o RequestTTY
	quiet   bool     // -q: ssh quiet, and no 2o progress
}

// OpenSSH flags (getopt string of OpenSSH 9/10).
const (
	sshValueFlags   = "BbcDEeFIiJLlmOoPpQRSWw"
	sshBoolFlags    = "1246AaCfGgKkMNnqsTtVvXxYy"
	sshSessionFlags = "AaDeLnRXxY" // per session: forwards, stdin, escape char
	sshBothFlags    = "EFoqv"      // configuration and logging apply to both
)

// Modes 2o cannot wrap: it needs one interactive session it controls.
var sshUnsupported = map[byte]string{
	'N': "-N (no remote command)", 'f': "-f (go to background)", 'W': "-W (stdio forwarding)",
	'O': "-O (control commands)", 'Q': "-Q (query)", 'G': "-G (print configuration)",
	'V': "-V (version)", 's': "-s (subsystem)", 'M': "-M (connection master)", 'S': "-S (control socket)",
}

func sshUsage() *userError {
	return fail("usage: 2o ssh [ssh options] HOST [command ...]").exit(exitUsage)
}

// parseSSH splits ssh's command line like ssh does: options (grouped -4v,
// glued -p2222), the host, options right after the host, then the command.
func parseSSH(args []string) (sshOptions, error) {
	var o sshOptions
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			if o.host == "" && i < len(args) {
				o.host = args[i]
				i++
			}
			break
		}
		if len(a) < 2 || a[0] != '-' {
			if o.host != "" {
				break // the command starts here
			}
			o.host = a
			continue
		}
		for j := 1; j < len(a); j++ {
			f := a[j]
			if what, bad := sshUnsupported[f]; bad {
				return o, fail("2o ssh cannot wrap %s", what).hint("use plain ssh for that").exit(exitUsage)
			}
			if strings.IndexByte(sshValueFlags, f) >= 0 {
				v := a[j+1:]
				if v == "" {
					if i+1 >= len(args) {
						return o, fail("ssh option -%c needs a value", f).hint("usage: 2o ssh [ssh options] HOST [command ...]").exit(exitUsage)
					}
					i++
					v = args[i]
				}
				if f == 'o' {
					if err := o.config(v); err != nil {
						return o, err
					}
				}
				o.add(f, v)
				break
			}
			switch {
			case strings.IndexByte(sshBoolFlags, f) < 0:
				return o, fail("unknown ssh option -%c", f).hint("usage: 2o ssh [ssh options] HOST [command ...]").exit(exitUsage)
			case f == 't':
				if !strings.HasPrefix(o.tty, "-t") {
					o.tty = "-"
				}
				o.tty += "t"
			case f == 'T':
				o.tty = "-T"
			default:
				o.quiet = o.quiet || f == 'q'
				o.add(f, "")
			}
		}
	}
	if o.host == "" {
		return o, sshUsage()
	}
	o.command = args[i:]
	return o, nil
}

func (o *sshOptions) add(f byte, v string) {
	a := []string{"-" + string(f)}
	if v != "" {
		a = append(a, v)
	}
	switch {
	case strings.IndexByte(sshBothFlags, f) >= 0:
		o.master = append(o.master, a...)
		o.session = append(o.session, a...)
	case strings.IndexByte(sshSessionFlags, f) >= 0:
		o.session = append(o.session, a...)
	default:
		o.master = append(o.master, a...)
	}
}

// config vets one -o keyword=value.
func (o *sshOptions) config(v string) error {
	key, _, _ := strings.Cut(strings.TrimSpace(v), "=")
	key = strings.ToLower(strings.Fields(key + " x")[0])
	switch key {
	case "sessiontype", "forkafterauthentication":
		return fail("2o ssh cannot wrap -o %s", v).hint("use plain ssh for that").exit(exitUsage)
	case "remotecommand":
		return fail("2o ssh cannot wrap -o %s", v).hint("give the command after the host: 2o ssh HOST COMMAND").exit(exitUsage)
	case "controlmaster", "controlpath", "controlpersist":
		return fail("2o ssh cannot wrap -o %s", v).hint("2o runs its own private connection master").exit(exitUsage)
	case "requesttty":
		o.ttyConf = true
	}
	return nil
}

// ttyFlag is the PTY choice for the user's session: the user's own, else a
// terminal for an interactive shell and none for a command.
func (o *sshOptions) ttyFlag() string {
	switch {
	case o.tty != "":
		return o.tty
	case o.ttyConf:
		return ""
	case len(o.command) == 0:
		return "-t"
	}
	return "-T"
}

func daemonLog() string { return filepath.Join(cacheDir(), "daemon.log") }

// ensureDaemon returns the control socket of the desktop daemon, starting it
// when none answers. It prints nothing: holdSession calls it in the background.
func ensureDaemon() (string, error) {
	p, e := socketPath()
	if e != nil {
		return "", e
	}
	if daemonAlive(p) {
		return p, nil
	}
	self, e := os.Executable()
	if e != nil {
		return "", e
	}
	// Detach: own session (survives the terminal closing) and its own log,
	// so a piped `2o ssh ... | cmd` does not wait for the daemon to exit.
	if e = os.MkdirAll(cacheDir(), 0700); e != nil {
		return "", e
	}
	logf, e := os.OpenFile(daemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return "", e
	}
	_ = cmd.Process.Release()
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		if daemonAlive(p) {
			return p, nil
		}
	}
	return "", errors.New("the desktop daemon did not answer within 2s")
}

func daemonAlive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// selfExecutable is the running binary; replaced in tests.
var selfExecutable = os.Executable

// remoteArch maps `uname -m` to a Go architecture 2o ships.
func remoteArch(machine string) (string, bool) {
	switch machine {
	case "x86_64", "amd64":
		return "amd64", true
	case "aarch64", "arm64":
		return "arm64", true
	}
	return "", false
}

func binaryCache(goos, goarch string) string {
	return filepath.Join(cacheDir(), "bin", "2o-"+goos+"-"+goarch)
}

// remoteBinary returns a 2o binary for goos/goarch: a prebuilt one, else one
// built (once, cached) from the source checkout in the current directory.
func remoteBinary(goos, goarch string) (string, error) {
	if p, ok := prebuiltBinary(goos, goarch); ok {
		return p, nil
	}
	return buildBinary(goos, goarch)
}

// prebuiltBinary looks, in order, for the running executable when it
// matches, then 2o-GOOS-GOARCH next to it, in ../lib/2o (packages), and in
// the user cache ($XDG_CACHE_HOME/2o/bin).
func prebuiltBinary(goos, goarch string) (string, bool) {
	var candidates []string
	if self, err := selfExecutable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			return self, true
		}
		name, dir := "2o-"+goos+"-"+goarch, filepath.Dir(self)
		candidates = append(candidates, filepath.Join(dir, name), filepath.Join(dir, "..", "lib", "2o", name))
	}
	for _, p := range append(candidates, binaryCache(goos, goarch)) {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return p, true
		}
	}
	return "", false
}

// buildBinary cross-builds 2o into the user cache, only from this project's
// source checkout (an unrelated directory must never be built as 2o).
func buildBinary(goos, goarch string) (string, error) {
	target := binaryCache(goos, goarch)
	missing := fail("no 2o binary for %s/%s (the server's CPU)", goos, goarch).
		hint("install a 2o release that bundles all architectures").
		hint("or place it at %s", target).
		hint("or run 2o ssh from the 2o source checkout with Go installed").exit(exitUsage)
	manifest, err := os.ReadFile("go.mod")
	if err != nil || !bytes.HasPrefix(manifest, []byte("module github.com/arthurnbr/2o\n")) {
		return "", missing
	}
	if _, err = exec.LookPath("go"); err != nil {
		return "", missing.because(err)
	}
	s := ui.Start("building 2o for %s/%s (one-time)", goos, goarch)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		s.Fail()
		return "", fail("cannot create %s", filepath.Dir(target)).because(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".build-*")
	if err != nil {
		s.Fail()
		return "", fail("cannot write to %s", filepath.Dir(target)).because(err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	var out bytes.Buffer
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", tmp.Name(), ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err = cmd.Run(); err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		s.Fail()
		e := fail("could not build 2o for %s/%s", goos, goarch).because(err)
		if l := lastLine(out.String()); l != "" {
			e.hint("go said: %s", l)
		}
		return "", e
	}
	s.Done()
	return target, nil
}

func fileDigest(p string) (sum string, size int64, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), size, err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// remoteError is a bootstrap command that failed on the server.
type remoteError struct {
	code   int
	stderr string
}

func (e *remoteError) Error() string {
	if e.stderr != "" {
		return e.stderr
	}
	return fmt.Sprintf("exit status %d", e.code)
}

// withServer adds what the server printed (a few lines) as hints.
func withServer(u *userError, host string, err error) *userError {
	var re *remoteError
	if errors.As(err, &re) && re.stderr != "" {
		lines := strings.Split(re.stderr, "\n")
		for _, l := range lines[max(0, len(lines)-3):] {
			if l = strings.TrimSpace(l); l != "" {
				u.hint("%s: %s", host, l)
			}
		}
	}
	return u.because(err)
}

// sshConn is a private OpenSSH connection master: authentication happens
// once, every later ssh goes through its control socket.
type sshConn struct {
	host string
	dir  string // private local directory holding the control socket
	ctl  string
	ctx  context.Context // cancelled when the user interrupts the bootstrap
}

// masterLinger is how long the master outlives its last client. 2o closes it
// explicitly; this only bounds a master left behind when 2o is killed, and
// must cover the gaps between bootstrap steps (a one-time cross build).
const masterLinger = "120"

// connectSSH authenticates once with the user's connection options. ssh -f
// returns when the master is authenticated and has gone to the background
// in its own session, so the terminal's Ctrl-C never reaches it.
func connectSSH(ctx context.Context, o sshOptions) (*sshConn, error) {
	dir, err := os.MkdirTemp("", "2o-ssh-")
	if err != nil {
		return nil, fail("cannot create a private directory for ssh").because(err)
	}
	c := &sshConn{host: o.host, dir: dir, ctl: filepath.Join(dir, "cm"), ctx: ctx}
	// 2o's settings come first: for -o, the first value obtained wins.
	a := []string{"-M", "-S", c.ctl, "-f", "-N", "-o", "ControlPersist=" + masterLinger,
		"-o", "ClearAllForwardings=yes", "-o", "RemoteCommand=none"}
	cmd := exec.CommandContext(ctx, "ssh", append(append(a, o.master...), o.host)...)
	// Password and host key prompts use the terminal; errors go to stderr.
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	if err = cmd.Run(); err != nil {
		os.RemoveAll(dir)
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fail("cannot run ssh").hint("install OpenSSH's ssh client").because(err)
		}
		return nil, fail("could not connect to %s", o.host).
			hint("check that plain `ssh %s` works", o.host).because(err).exit(exitSSH)
	}
	return c, nil
}

// client is an ssh command through the master. ProxyCommand=false makes it
// fail instead of opening a second, unauthenticated-by-us connection.
func (c *sshConn) client(a ...string) *exec.Cmd {
	base := []string{"-F", "/dev/null", "-S", c.ctl, "-o", "ControlMaster=no", "-o", "ProxyCommand=false", "-o", "BatchMode=yes"}
	return exec.CommandContext(c.ctx, "ssh", append(base, a...)...)
}

// run runs a bootstrap script on the server: no user option, no PTY.
func (c *sshConn) run(script string, stdin io.Reader) (string, error) {
	return c.output(c.client("-T", c.host, script), stdin)
}

func (c *sshConn) output(cmd *exec.Cmd, stdin io.Reader) (string, error) {
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return "", &remoteError{code: code, stderr: strings.TrimSpace(errb.String())}
	}
	return strings.TrimSpace(out.String()), nil
}

// forward adds a reverse forward from a server loopback port chosen by sshd
// to target, and returns that port. sshd binds it before this returns.
func (c *sshConn) forward(target string) (string, error) {
	port, err := c.output(c.client("-O", "forward", "-R", "127.0.0.1:0:"+target, c.host), nil)
	if err != nil {
		return "", err
	}
	if port == "" || strings.Trim(port, "0123456789") != "" {
		return "", fmt.Errorf("unexpected port %q", port)
	}
	return port, nil
}

// close stops the master (it closes the forward and any leftover channel).
func (c *sshConn) close() {
	c.ctx = context.Background() // cleanup runs even after an interrupt
	_ = c.client("-O", "exit", c.host).Run()
	os.RemoveAll(c.dir)
}

// remoteTmp holds the per-session directories on the server; a var for tests.
var remoteTmp = "/tmp"

const remoteBin = `"$HOME/.local/bin/2o"`

// probeScript prints: OS, CPU, uid, then (if 2o is installed) its sha256 and
// the line from `2o version-info` (empty for builds older than that command).
const probeScript = `uname -s; uname -m; id -u; b="$HOME/.local/bin/2o"; if test -f "$b"; then sha256sum "$b" | cut -d ' ' -f 1; "$b" version-info 2>/dev/null </dev/null | head -n 1; fi`

// uploadScript installs stdin as ~/.local/bin/2o: a unique temporary file
// (mktemp, created exclusively), checked against sum, then renamed over the
// old binary (rename replaces a symlink, it never writes through it). A failed
// or concurrent upload never leaves a partial binary.
func uploadScript(sum string) string {
	return `set -e; umask 077; d="$HOME/.local/bin"; mkdir -p "$d"
if test -d "$d/2o"; then echo "$d/2o is a directory" >&2; exit 1; fi
t=$(mktemp "$d/.2o.XXXXXXXX"); trap 'rm -f -- "$t"' EXIT
cat > "$t"
test "$(sha256sum < "$t" | cut -d ' ' -f 1)" = ` + quote(sum) + ` || { echo "upload of 2o was corrupted" >&2; exit 1; }
chmod 700 "$t"; mv -f -- "$t" "$d/2o"; trap - EXIT`
}

// setupScript creates the session directory exclusively (mkdir fails if
// anything exists there, links included), checks it is ours and private,
// writes the session file from stdin without clobbering, puts the xdg-open
// shim in it, removes the ~/.local/bin/xdg-open link older versions created,
// and checks the tunnel end to end. On failure the directory is removed.
func setupScript(dir, desktop string) string {
	return `set -e; umask 077; d=` + quote(dir) + `
mkdir -m 700 -- "$d"; trap 'rm -rf -- "$d"' EXIT
if test -L "$d" || test "$(stat -c %u:%a -- "$d")" != "$(id -u):700"; then echo "unsafe session directory $d" >&2; exit 1; fi
(set -C; cat > "$d/session")
ln -s -- "$HOME/.local/bin/2o" "$d/xdg-open"
l="$HOME/.local/bin/xdg-open"; if test -L "$l"; then case "$(readlink -- "$l")" in 2o|*/2o) rm -f -- "$l";; esac; fi
TWO_O_SESSION="$d/session" TWO_O_DESKTOP=` + quote(desktop) + ` TWO_O_QUIET=1 ` + remoteBin + ` status
trap - EXIT`
}

// sessionScript runs the user's shell or command with the session's
// environment; the xdg-open shim is on PATH for this session only.
func sessionScript(dir, desktop string, command []string) string {
	s := "TWO_O_SESSION=" + quote(dir+"/session") + "; TWO_O_DESKTOP=" + quote(desktop) +
		"; BROWSER=" + quote(dir+"/xdg-open") + "; PATH=" + quote(dir) + `:"$HOME/.local/bin:$PATH"` +
		"; export TWO_O_SESSION TWO_O_DESKTOP BROWSER PATH; "
	if len(command) == 0 {
		return s + "exec " + remoteBin + " shell"
	}
	return s + strings.Join(command, " ")
}

func sshSession(args []string) error {
	o, err := parseSSH(args)
	if err != nil {
		return err
	}
	if o.quiet {
		ui.SetQuiet(true)
	}
	// 2o must outlive Ctrl-C and termination requests to clean up (session
	// directory, held secret, connection master): during the bootstrap they
	// cancel it, during the session they are relayed to ssh.
	sig := newSignalRelay()
	defer sig.stop()

	ui.Header("2o → %s", o.host)
	sock, err := startDaemon()
	if err != nil {
		return err
	}
	master, daemonAddr, err := readKey(keyPath(sock))
	if err != nil {
		return fail("cannot read the desktop daemon's key").hint("see its log: %s", daemonLog()).because(err)
	}

	// A static line: ssh may prompt for a password or host key right here.
	step := ui.Begin("connecting to %s", o.host)
	c, err := connectSSH(sig.ctx, o)
	if err != nil {
		step.Fail()
		return sig.interrupted(err)
	}
	step.Done()
	defer c.close()

	uid, err := installRemote(c)
	if err != nil {
		return sig.interrupted(err)
	}
	step = ui.Start("opening tunnel")
	desktop := desktopLabel()
	dir, release, err := openTunnel(c, sock, master, daemonAddr, uid, desktop)
	if err != nil {
		step.Fail()
		return sig.interrupted(err)
	}
	defer release()
	defer func() { c.ctx = context.Background(); _, _ = c.run("rm -rf -- "+quote(dir), nil) }()
	step.Done()
	ui.Success("desktop tunnel ready — files, links and clipboard now open on %s", desktop)

	err = runSession(c, o, sessionScript(dir, desktop, o.command), sig)
	ui.Note("tunnel closed")
	return err
}

// signalRelay catches SIGINT, SIGQUIT, SIGTERM and SIGHUP for the whole
// session. Until the session's ssh runs, any of them cancels ctx; then
// SIGTERM/SIGHUP are passed to it (the terminal sends Ctrl-C to it directly).
type signalRelay struct {
	ctx    context.Context
	cancel context.CancelFunc
	sigs   chan os.Signal
	done   chan struct{}
	mu     sync.Mutex
	got    os.Signal
	child  *os.Process
}

func newSignalRelay() *signalRelay {
	ctx, cancel := context.WithCancel(context.Background())
	r := &signalRelay{ctx: ctx, cancel: cancel, sigs: make(chan os.Signal, 4), done: make(chan struct{})}
	signal.Notify(r.sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for {
			select {
			case s := <-r.sigs:
				r.mu.Lock()
				if r.child == nil {
					if r.got == nil {
						r.got = s
					}
					r.cancel()
				} else if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = r.child.Signal(s)
				}
				r.mu.Unlock()
			case <-r.done:
				return
			}
		}
	}()
	return r
}

func (r *signalRelay) attach(p *os.Process) { r.mu.Lock(); r.child = p; r.mu.Unlock() }

func (r *signalRelay) stop() { signal.Stop(r.sigs); close(r.done); r.cancel() }

// interrupted turns a bootstrap failure caused by a signal into the usual
// 128+signal exit status; other errors are returned unchanged.
func (r *signalRelay) interrupted(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.got.(syscall.Signal); ok {
		ui.Note("interrupted")
		return exitStatus(128 + int(s))
	}
	return err
}

// startDaemon makes sure this desktop's daemon runs, and offers to restart
// one left running by another 2o build (e.g. after an update).
func startDaemon() (string, error) {
	s := ui.Start("starting desktop daemon")
	p, err := socketPath()
	if err == nil && daemonAlive(p) {
		info, sessions, ierr := daemonInfo(p)
		if ierr != nil || compareBuilds(info, thisBuild()) == sameBuild {
			s.Skip("desktop daemon already running")
			return p, nil
		}
		s.Skip("desktop daemon already running")
		if !askRestartDaemon(info, sessions) {
			return p, nil
		}
		s = ui.Start("restarting desktop daemon with 2o %s", version)
		if err = stopDaemon(p); err != nil {
			s.Fail()
			return "", fail("could not stop the old desktop daemon").hint("stop it yourself: pkill -f '2o daemon'").because(err)
		}
	}
	p, err = restartDaemon()
	if err != nil {
		s.Fail()
		return "", fail("could not start the desktop daemon").hint("see its log: %s", daemonLog()).because(err)
	}
	s.Done()
	return p, nil
}

// askRestartDaemon reports the running daemon's version and asks to replace
// it with this 2o. Its open sessions survive: they re-attach to the new one.
func askRestartDaemon(info buildInfo, sessions int) bool {
	if compareBuilds(info, thisBuild()) == peerNewer {
		ui.Warn("the desktop daemon runs a newer 2o (%s) than this one (%s)", info.label(thisBuild()), thisBuild().label(info))
		return confirmUpdate("restart it with this older 2o?", false)
	}
	ui.Warn("older desktop daemon detected: %s (this 2o is %s)", info.label(thisBuild()), thisBuild().label(info))
	q := "restart it with 2o " + version + "?"
	if sessions > 0 {
		q = fmt.Sprintf("restart it with 2o %s? (%d open session%s will reconnect)", version, sessions, plural(sessions))
	}
	if confirmUpdate(q, true) {
		return true
	}
	ui.Note("keeping the running desktop daemon %s", info.label(thisBuild()))
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// daemonInfo asks the running daemon for its build and held sessions.
func daemonInfo(sock string) (buildInfo, int, error) {
	m, err := controlCall(sock, "info")
	if err != nil {
		return buildInfo{}, 0, err
	}
	return parseBuildInfo(m.Version), int(m.Size), nil
}

// stopDaemon asks the running daemon to exit and waits until it is gone.
// Another 2o (a session re-holding) may start a new daemon at the same path
// meanwhile, so "gone" means a new daemon's key is published, or no key and
// no answer on the socket: close removes the key before the socket stops
// accepting, so a missing key alone does not mean the old daemon has exited.
func stopDaemon(sock string) error {
	oldMaster, _, err := readKey(keyPath(sock))
	if err != nil {
		return err
	}
	if _, err = controlCall(sock, "shutdown"); err != nil {
		return err
	}
	for range 100 {
		master, _, err := readKey(keyPath(sock))
		if err == nil && master != oldMaster || err != nil && !daemonAlive(sock) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("the daemon is still running after 2s")
}

// controlCall sends one master-authenticated op on the control socket.
func controlCall(sock, op string) (wire.Message, error) {
	master, _, err := readKey(keyPath(sock))
	if err != nil {
		return wire.Message{}, err
	}
	c, err := net.DialTimeout("unix", sock, dialTimeout)
	if err != nil {
		return wire.Message{}, err
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return wire.Message{}, err
	}
	if err = wire.Write(c, wire.Message{Op: op, Token: master}); err != nil {
		return wire.Message{}, err
	}
	m, err := wire.Read(c)
	if err == nil && m.Error != "" {
		err = errors.New(m.Error)
	}
	return m, err
}

// installRemote makes ~/.local/bin/2o on the server match the binary for its
// CPU, and returns the server-side user id. A different installed build is
// reported with its version and replaced only if the user agrees (or
// TWO_O_UPDATE says so); an incompatible one must be replaced to continue.
func installRemote(c *sshConn) (string, error) {
	step := ui.Start("checking 2o on %s", c.host)
	info, err := c.run(probeScript, nil)
	if err != nil {
		step.Fail()
		return "", withServer(fail("could not inspect %s", c.host), c.host, err)
	}
	lines := strings.Split(info, "\n")
	if len(lines) < 3 || strings.Trim(lines[2], "0123456789") != "" || lines[2] == "" {
		step.Fail()
		return "", fail("unexpected answer from %s", c.host).hint("a login script may print text; got: %q", info)
	}
	if lines[0] != "Linux" {
		step.Fail()
		return "", fail("%s runs %s; 2o supports Linux servers", c.host, lines[0]).exit(exitUsage)
	}
	arch, ok := remoteArch(lines[1])
	if !ok {
		step.Fail()
		return "", fail("%s has a %s CPU; 2o supports amd64 and arm64 servers", c.host, lines[1]).exit(exitUsage)
	}
	bin, found := prebuiltBinary("linux", arch)
	if !found {
		step.Done()
		if bin, err = buildBinary("linux", arch); err != nil {
			return "", err
		}
		step = ui.Start("checking 2o on %s", c.host)
	}
	sum, size, err := fileDigest(bin)
	if err != nil {
		step.Fail()
		return "", fail("cannot read %s", bin).because(err)
	}
	installedSum, installedInfo := "", ""
	if len(lines) > 3 {
		installedSum = lines[3]
	}
	if len(lines) > 4 {
		installedInfo = lines[4]
	}
	remote := parseBuildInfo(installedInfo)
	_, isRelease := releaseVersion(version)
	switch {
	case installedSum == sum, isRelease && compareBuilds(remote, thisBuild()) == sameBuild:
		step.Skip("2o %s is up to date on %s", version, c.host)
		return lines[2], nil
	case installedSum == "":
		step.Update("installing 2o %s on %s (%s)", version, c.host, humanSize(size))
	default:
		update, err := askRemoteUpdate(step, c.host, remote)
		if err != nil || !update {
			return lines[2], err
		}
		step = ui.Start("updating 2o on %s: %s → %s (%s)", c.host, remote.label(thisBuild()), thisBuild().label(remote), humanSize(size))
	}
	f, err := os.Open(bin)
	if err != nil {
		step.Fail()
		return "", fail("cannot read %s", bin).because(err)
	}
	defer f.Close()
	if _, err = c.run(uploadScript(sum), f); err != nil {
		step.Fail()
		return "", withServer(fail("could not install 2o on %s", c.host), c.host, err)
	}
	step.Done()
	return lines[2], nil
}

// askRemoteUpdate ends step with what was found on the server and asks
// whether to replace it with this desktop's build. A build speaking another
// protocol cannot work with this one: declining is then an error.
func askRemoteUpdate(step *ui.Step, host string, remote buildInfo) (bool, error) {
	step.Skip("checked 2o on %s", host)
	switch compareBuilds(remote, thisBuild()) {
	case incompatible:
		ui.Warn("%s has 2o %s, which cannot work with this desktop's 2o %s", host, remote, version)
		if confirmUpdate(fmt.Sprintf("update 2o on %s to %s? (required)", host, version), true) {
			return true, nil
		}
		return false, fail("2o on %s was not updated", host).
			hint("its version cannot work with this desktop's 2o %s", version).
			hint("run 2o ssh %s again and answer y, or set TWO_O_UPDATE=always", host).exit(exitUsage)
	case peerNewer:
		ui.Warn("%s has a newer 2o (%s) than this desktop (%s)", host, remote.label(thisBuild()), thisBuild().label(remote))
		if confirmUpdate(fmt.Sprintf("replace it with this desktop's older %s?", version), false) {
			return true, nil
		}
		ui.Note("keeping 2o %s on %s; update 2o on this desktop to match", remote, host)
		return false, nil
	case sameBuild:
		ui.Warn("%s has another build of 2o %s", host, version)
	default:
		if remote.version == "" {
			ui.Warn("older 2o detected on %s (this desktop has %s)", host, version)
		} else {
			ui.Warn("older 2o detected on %s: %s (this desktop has %s)", host, remote.label(thisBuild()), thisBuild().label(remote))
		}
	}
	if confirmUpdate(fmt.Sprintf("update 2o on %s to %s?", host, version), true) {
		return true, nil
	}
	ui.Note("keeping the current 2o on %s; some commands may be missing or behave differently", host)
	return false, nil
}

// openTunnel holds a new session on the desktop daemon, forwards a server
// port to it, writes the session directory, and checks the tunnel works.
func openTunnel(c *sshConn, sock, master, daemonAddr, uid, desktop string) (dir string, release func(), err error) {
	id, err := randomHex(12)
	if err != nil {
		return "", nil, err
	}
	token, err := randomHex(32)
	if err != nil {
		return "", nil, err
	}
	release, err = holdSession(sock, master, id, token, desktop, holdNotice(c.host))
	if err != nil {
		return "", nil, fail("the desktop daemon refused the session").hint("see its log: %s", daemonLog()).because(err)
	}
	port, err := c.forward(daemonAddr)
	if err != nil {
		release()
		return "", nil, withServer(fail("could not open the tunnel to %s", c.host).
			hint("the server must allow remote forwarding (AllowTcpForwarding in sshd_config)"), c.host, err)
	}
	dir = fmt.Sprintf("%s/2o-%s-%s", remoteTmp, uid, id)
	file := "127.0.0.1:" + port + " " + id + " " + token + " " + desktop + "\n" // the token travels on stdin, never in argv
	if _, err = c.run(setupScript(dir, desktop), strings.NewReader(file)); err != nil {
		release()
		u := fail("could not prepare the session on %s", c.host)
		if re := (*remoteError)(nil); errors.As(err, &re) && re.code == exitNoTunnel {
			u = fail("the tunnel to the desktop does not work").exit(exitNoTunnel)
		}
		return "", nil, withServer(u, c.host, err)
	}
	return dir, release, nil
}

// holdNotice tells the user, during the session, when the desktop daemon
// went away and whether the tunnel came back.
func holdNotice(host string) func(error) {
	return func(err error) {
		switch {
		case err == nil:
			ui.Info("desktop daemon is back, tunnel restored")
		case errors.Is(err, errHoldFailed):
			ui.Warn("desktop tunnel lost: files, links and clipboard no longer reach the desktop")
			ui.Hint("reconnect: 2o ssh %s", host)
		default:
			ui.Warn("desktop daemon stopped, waiting for it to come back")
		}
	}
}

// runSession runs the user's session once through the master and returns its
// exit code as exitStatus (255 when ssh itself failed, as with plain ssh).
func runSession(c *sshConn, o sshOptions, script string, sig *signalRelay) error {
	a := []string{"-S", c.ctl, "-o", "ControlMaster=no", "-o", "ProxyCommand=false", "-o", "RemoteCommand=none"}
	a = append(a, o.session...)
	if t := o.ttyFlag(); t != "" {
		a = append(a, t)
	}
	cmd := exec.Command("ssh", append(a, o.host, script)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fail("cannot run ssh").because(err)
	}
	sig.attach(cmd.Process)
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return exitStatus(128 + int(ws.Signal()))
		}
		return exitStatus(ee.ExitCode())
	}
	if err != nil {
		return fail("ssh failed").because(err).exit(exitSSH)
	}
	return nil
}
