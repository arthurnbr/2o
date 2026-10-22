package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/arthurnbr/2o/internal/ui"
)

func TestParseSSH(t *testing.T) {
	for _, tc := range []struct {
		args                  string
		host, master, session string
		command, tty          string
		quiet                 bool
	}{
		{args: "-J bastion -p 222 -o ProxyCommand=none user@tailnet 2o notify done ok",
			host: "user@tailnet", master: "-J bastion -p 222 -o ProxyCommand=none", session: "-o ProxyCommand=none",
			command: "2o notify done ok", tty: "-T"},
		{args: "-p2222 host", host: "host", master: "-p 2222", tty: "-t"},
		{args: "-vp 2222 host", host: "host", master: "-v -p 2222", session: "-v", tty: "-t"},
		{args: "-4v host", host: "host", master: "-4 -v", session: "-v", tty: "-t"},
		{args: "-iid_ed25519 -lroot host", host: "host", master: "-i id_ed25519 -l root", tty: "-t"},
		// Forwards and stdin handling belong to the user's session only.
		{args: "-L 8080:localhost:80 server", host: "server", session: "-L 8080:localhost:80", tty: "-t"},
		{args: "-R9000:localhost:9000 -D 1080 -n -e none host cmd", host: "host",
			session: "-R 9000:localhost:9000 -D 1080 -n -e none", command: "cmd", tty: "-T"},
		// Options after the host are still options; the command starts at the next word.
		{args: "host -p 2222 ls -la", host: "host", master: "-p 2222", command: "ls -la", tty: "-T"},
		{args: "-- host -p", host: "host", command: "-p", tty: "-T"},
		// PTY: the user's choice wins over the default.
		{args: "-t host top", host: "host", command: "top", tty: "-t"},
		{args: "-tt host", host: "host", tty: "-tt"},
		{args: "-T host", host: "host", tty: "-T"},
		{args: "-o RequestTTY=yes host cmd", host: "host", master: "-o RequestTTY=yes", session: "-o RequestTTY=yes", command: "cmd"},
		{args: "-q host", host: "host", master: "-q", session: "-q", tty: "-t", quiet: true},
	} {
		t.Run(tc.args, func(t *testing.T) {
			o, err := parseSSH(strings.Fields(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			got := []string{o.host, strings.Join(o.master, " "), strings.Join(o.session, " "), strings.Join(o.command, " "), o.ttyFlag()}
			want := []string{tc.host, tc.master, tc.session, tc.command, tc.tty}
			if strings.Join(got, "|") != strings.Join(want, "|") || o.quiet != tc.quiet {
				t.Fatalf("got host|master|session|command|tty = %q quiet=%v, want %q quiet=%v", got, o.quiet, want, tc.quiet)
			}
		})
	}
}

func TestParseSSHRejects(t *testing.T) {
	for _, args := range []string{
		"-N host", "-f host", "-fN host", "-W localhost:22 host", "-O check host", "-Q cipher",
		"-G host", "-V", "-s host sftp", "-M host", "-S /tmp/cm host",
		"-o ControlPath=/tmp/x host", "-o RemoteCommand=ls host", "-oSessionType=none host",
		"-p", "-Z host", "", "-v",
	} {
		_, err := parseSSH(strings.Fields(args))
		var ue *userError
		if !errors.As(err, &ue) || exitCode(err) != exitUsage {
			t.Errorf("%q: got %v, want a usage error (exit %d)", args, err, exitUsage)
		}
	}
}

// fakeServer puts a fake `ssh` on PATH that runs "remote" scripts with the
// local sh in a throwaway HOME, records every invocation, and serves the
// connection-master control commands. The desktop daemon is real.
type fakeServer struct {
	home, log, tmp string
}

const fakeSSH = `#!/bin/sh
printf '%s\n' "$1" >> "$FAKE_LOG"
for a; do last=$a; done
case "$1" in
-M) exit ${FAKE_MASTER_EXIT:-0};;
esac
case " $* " in
*" -O forward "*) echo 40000; exit 0;;
*" -O exit "*) exit 0;;
esac
HOME="$FAKE_HOME" exec sh -c "$last"
`

// fake2o stands in for the uploaded binary on the "server".
const fake2o = `#!/bin/sh
case "$1" in
status) test -f "$TWO_O_SESSION" || exit 9; exit ${FAKE_STATUS:-0};;
shell) exit 0;;
esac
exit 1
`

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	root := t.TempDir()
	f := &fakeServer{home: filepath.Join(root, "home"), log: filepath.Join(root, "ssh.log"), tmp: filepath.Join(root, "tmp")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{f.home, f.tmp, bin} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(bin, "ssh"), fakeSSH)
	self := filepath.Join(root, "desk", "2o")
	if err := os.Mkdir(filepath.Dir(self), 0700); err != nil {
		t.Fatal(err)
	}
	writeExec(t, self, fake2o)
	oldSelf, oldTmp := selfExecutable, remoteTmp
	selfExecutable = func() (string, error) { return self, nil }
	remoteTmp = f.tmp
	t.Cleanup(func() { selfExecutable, remoteTmp = oldSelf, oldTmp })
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", f.log)
	t.Setenv("FAKE_HOME", f.home)
	t.Setenv("FAKE_MASTER_EXIT", "")
	t.Setenv("FAKE_STATUS", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("TWO_O_QUIET", "")
	// A real daemon where 2o ssh looks for it.
	t.Setenv("XDG_RUNTIME_DIR", daemonDir(t))
	sock, err := socketPath()
	if err != nil {
		t.Fatal(err)
	}
	runDaemon(t, sock)
	return f
}

func writeExec(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0700); err != nil {
		t.Fatal(err)
	}
}

// calls returns the first argument of each ssh invocation.
func (f *fakeServer) calls(t *testing.T) []string {
	b, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func (f *fakeServer) sessionRuns(t *testing.T) int {
	n := 0
	for _, c := range f.calls(t) {
		if c == "-S" { // only the user's session starts with -S
			n++
		}
	}
	return n
}

// captureUI sends progress to a buffer, as when stderr is not a terminal.
func captureUI(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	ui.SetOutput(&b)
	ui.SetQuiet(false)
	t.Cleanup(func() { ui.SetOutput(os.Stderr); ui.SetQuiet(false) })
	return &b
}

func TestSSHCommandRunsOnceAndPassesExitCode(t *testing.T) {
	for _, code := range []int{0, 42, 255} {
		f := newFakeServer(t)
		captureUI(t)
		err := sshSession([]string{"host", "exit", strconv.Itoa(code)})
		if code == 0 && err != nil || code != 0 && !errors.Is(err, exitStatus(code)) {
			t.Fatalf("exit %d: got %v", code, err)
		}
		if n := f.sessionRuns(t); n != 1 {
			t.Fatalf("exit %d: command ran %d times, want exactly once", code, n)
		}
		if c := f.calls(t); c[0] != "-M" || c[len(c)-1] != "-F" {
			t.Fatalf("want one master first and its exit last, got %q", c)
		}
		if left, _ := filepath.Glob(filepath.Join(f.tmp, "*")); len(left) != 0 {
			t.Fatalf("session directory left on the server: %q", left)
		}
	}
}

func TestSSHProgressAndUpToDate(t *testing.T) {
	f := newFakeServer(t)
	out := captureUI(t)
	if err := sshSession([]string{"myhost", "true"}); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	for _, want := range []string{"2o → myhost\n", "✓ connecting to myhost\n", "✓ installing 2o on myhost (", "✓ opening tunnel\n",
		"✓ desktop tunnel ready — files, links and clipboard now open on " + desktopLabel() + "\n", "tunnel closed\n"} {
		if !strings.Contains(first, want) {
			t.Fatalf("progress lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "\x1b") || strings.Contains(first, "\r") {
		t.Fatalf("escape codes in non-terminal output: %q", first)
	}
	installed, err := os.ReadFile(filepath.Join(f.home, ".local", "bin", "2o"))
	if err != nil || string(installed) != fake2o {
		t.Fatalf("installed binary %q, %v", installed, err)
	}
	out.Reset()
	if err := sshSession([]string{"myhost", "true"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "– 2o is up to date on myhost\n") || strings.Contains(out.String(), "installing") {
		t.Fatalf("second run should not reinstall:\n%s", out)
	}
	out.Reset()
	if err := sshSession([]string{"-q", "myhost", "true"}); err != nil || out.Len() != 0 {
		t.Fatalf("-q printed %q (err %v)", out, err)
	}
}

func TestSSHFailuresAreUserErrors(t *testing.T) {
	f := newFakeServer(t)
	captureUI(t)
	t.Setenv("FAKE_MASTER_EXIT", "255")
	err := sshSession([]string{"host", "true"})
	var ue *userError
	if !errors.As(err, &ue) || exitCode(err) != exitSSH || !strings.Contains(strings.Join(ue.hints, " "), "plain `ssh host`") {
		t.Fatalf("connection failure: %#v", err)
	}
	t.Setenv("FAKE_MASTER_EXIT", "")
	t.Setenv("FAKE_STATUS", "3")
	err = sshSession([]string{"host", "true"})
	if !errors.As(err, &ue) || exitCode(err) != exitNoTunnel {
		t.Fatalf("broken tunnel: %#v", err)
	}
	if n := f.sessionRuns(t); n != 0 {
		t.Fatalf("command ran %d times although the tunnel check failed", n)
	}
	if left, _ := filepath.Glob(filepath.Join(f.tmp, "*")); len(left) != 0 {
		t.Fatalf("session directory left after a failed setup: %q", left)
	}
}

// sh runs a bootstrap script locally as the server would, with HOME=home.
func sh(t *testing.T, home, script, stdin string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestUploadScriptIsAtomicAndUnique(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// The installed binary is a link whose target must never be written through.
	victim := filepath.Join(t.TempDir(), "victim")
	writeExec(t, victim, "keep")
	if err := os.Symlink(victim, filepath.Join(bin, "2o")); err != nil {
		t.Fatal(err)
	}
	if err := sh(t, home, uploadScript(digest("v1")), "v1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("upload wrote through a symlink: %q", b)
	}
	st, err := os.Lstat(filepath.Join(bin, "2o"))
	if b, _ := os.ReadFile(filepath.Join(bin, "2o")); err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0700 || string(b) != "v1" {
		t.Fatalf("installed %q mode %v, %v", b, st.Mode(), err)
	}
	// A corrupted upload fails and leaves the previous binary and no temporary.
	if err := sh(t, home, uploadScript(digest("v2")), "truncated"); err == nil {
		t.Fatal("corrupted upload accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "2o")); string(b) != "v1" {
		t.Fatalf("failed upload replaced the binary: %q", b)
	}
	// Concurrent uploads use distinct temporaries: all succeed.
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- sh(t, home, uploadScript(digest("v3")), "v3") }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent upload: %v", err)
		}
	}
	entries, _ := os.ReadDir(bin)
	if len(entries) != 1 {
		t.Fatalf("temporaries left in ~/.local/bin: %v", entries)
	}
}

func TestSetupScriptSessionDirIsExclusive(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(bin, "2o"), fake2o)
	t.Setenv("FAKE_STATUS", "")
	tmp := t.TempDir()

	// Taken by a directory, or by a link to an attacker's directory: refused,
	// and nothing written into it.
	taken := filepath.Join(tmp, "2o-1-taken")
	if err := os.Mkdir(taken, 0700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	link := filepath.Join(tmp, "2o-1-link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{taken, link} {
		if err := sh(t, home, setupScript(dir, "me@desk"), "secret\n"); err == nil {
			t.Fatalf("%s: setup accepted an existing path", dir)
		}
	}
	if e, _ := os.ReadDir(taken); len(e) != 0 {
		t.Fatalf("wrote into a pre-existing directory: %v", e)
	}
	if e, _ := os.ReadDir(other); len(e) != 0 {
		t.Fatalf("followed a link: %v", e)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("removed someone else's link: %v", err)
	}

	// The legacy ~/.local/bin/xdg-open → 2o link is removed; another is kept.
	if err := os.Symlink("2o", filepath.Join(bin, "xdg-open")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "2o-1-new")
	if err := sh(t, home, setupScript(dir, "me@desk"), "addr id token me@desk\n"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("session dir mode %v, %v", st.Mode(), err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "session")); err != nil || string(b) != "addr id token me@desk\n" {
		t.Fatalf("session file %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "xdg-open")); err != nil || target != filepath.Join(bin, "2o") {
		t.Fatalf("session xdg-open shim -> %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "xdg-open")); !os.IsNotExist(err) {
		t.Fatalf("legacy xdg-open link kept: %v", err)
	}
	if err := os.Symlink("/usr/bin/firefox", filepath.Join(bin, "xdg-open")); err != nil {
		t.Fatal(err)
	}
	if err := sh(t, home, setupScript(filepath.Join(tmp, "2o-1-again"), "me@desk"), "x\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "xdg-open")); err != nil {
		t.Fatalf("removed a user's own xdg-open: %v", err)
	}

	// A failing tunnel check removes the directory it created.
	t.Setenv("FAKE_STATUS", "3")
	failed := filepath.Join(tmp, "2o-1-fail")
	if err := sh(t, home, setupScript(failed, "me@desk"), "x\n"); err == nil {
		t.Fatal("setup ignored a failed tunnel check")
	}
	if _, err := os.Lstat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed setup left %s", failed)
	}
}

// otherArch is a supported architecture different from this machine's.
func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

func crossSetup(t *testing.T) (exeDir, cache string) {
	t.Helper()
	root := t.TempDir()
	exeDir = filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(exeDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(exeDir, "2o"), "self")
	old := selfExecutable
	selfExecutable = func() (string, error) { return filepath.Join(exeDir, "2o"), nil }
	t.Cleanup(func() { selfExecutable = old })
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Chdir(root) // not a 2o checkout
	return exeDir, filepath.Join(root, "cache", "2o", "bin")
}

func TestRemoteBinaryLookupOrder(t *testing.T) {
	arch := otherArch()
	name := "2o-linux-" + arch
	exeDir, cache := crossSetup(t)
	if runtime.GOOS == "linux" {
		if p, err := remoteBinary("linux", runtime.GOARCH); err != nil || p != filepath.Join(exeDir, "2o") {
			t.Fatalf("same arch: %q, %v", p, err)
		}
	}
	lib := filepath.Join(exeDir, "..", "lib", "2o")
	for _, d := range []string{cache, lib, exeDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(d, name), d)
	}
	for _, want := range []string{filepath.Join(exeDir, name), filepath.Join(lib, name), filepath.Join(cache, name)} {
		p, err := remoteBinary("linux", arch)
		if err != nil || filepath.Clean(p) != filepath.Clean(want) {
			t.Fatalf("got %q, %v; want %q", p, err, want)
		}
		os.Remove(want)
	}
	_, err := remoteBinary("linux", arch)
	var ue *userError
	if !errors.As(err, &ue) || exitCode(err) != exitUsage || !strings.Contains(ue.msg, "linux/"+arch) ||
		!strings.Contains(strings.Join(ue.hints, "\n"), filepath.Join(cache, name)) {
		t.Fatalf("missing binary: %#v", err)
	}
}

func TestRemoteBinaryBuildsOnceInCheckout(t *testing.T) {
	arch := otherArch()
	_, cache := crossSetup(t)
	if err := os.WriteFile("go.mod", []byte("module github.com/arthurnbr/2o\n\ngo 1.25\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A fake go that records builds and writes its -o output.
	bin := t.TempDir()
	log := filepath.Join(bin, "builds")
	writeExec(t, filepath.Join(bin, "go"), `#!/bin/sh
echo "$GOOS/$GOARCH" >> "`+log+`"
while test $# -gt 0; do if test "$1" = -o; then printf built > "$2"; fi; shift; done
`)
	t.Setenv("PATH", bin)
	captureUI(t)
	for range 2 {
		p, err := remoteBinary("linux", arch)
		if err != nil || p != filepath.Join(cache, "2o-linux-"+arch) {
			t.Fatalf("got %q, %v", p, err)
		}
	}
	if b, _ := os.ReadFile(log); string(b) != "linux/"+arch+"\n" {
		t.Fatalf("builds: %q, want exactly one", b)
	}
}
