package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func splitSSH(args []string) (opts []string, host string, command []string, err error) {
	value := map[string]bool{"-p": true, "-i": true, "-J": true, "-F": true, "-l": true, "-o": true, "-S": true, "-b": true, "-c": true, "-m": true, "-E": true, "-W": true}
	for i := 0; i < len(args); i++ {
		s := args[i]
		if s == "--" {
			i++
			if i < len(args) {
				return opts, args[i], args[i+1:], nil
			}
			break
		}
		if strings.HasPrefix(s, "-") {
			opts = append(opts, s)
			if value[s] {
				i++
				if i >= len(args) {
					return nil, "", nil, errors.New("missing SSH option value")
				}
				opts = append(opts, args[i])
			}
			continue
		}
		return opts, s, args[i+1:], nil
	}
	return nil, "", nil, errors.New("usage: 2o ssh [ssh options] HOST [command ...]")
}
func sshRun(opts []string, host string, script string, input io.Reader) (string, error) {
	a := append(append([]string{}, opts...), "-T", host, script)
	cmd := exec.Command("ssh", a...)
	cmd.Stdin = input
	cmd.Stderr = os.Stderr
	b, e := cmd.Output()
	return strings.TrimSpace(string(b)), e
}
func ensureDaemon() (string, error) {
	p, e := socketPath()
	if e != nil {
		return "", e
	}
	c, e := net.Dial("unix", p)
	if e == nil {
		c.Close()
		return p, nil
	}
	self, e := os.Executable()
	if e != nil {
		return "", e
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if e = cmd.Start(); e != nil {
		return "", e
	}
	_ = cmd.Process.Release()
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		c, e = net.Dial("unix", p)
		if e == nil {
			c.Close()
			return p, nil
		}
	}
	return "", errors.New("local daemon failed to start")
}
func remoteBinary(arch string) (string, func(), error) {
	if arch != "amd64" && arch != "arm64" {
		return "", nil, fmt.Errorf("unsupported remote Linux architecture: %s", arch)
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		self, err := os.Executable()
		return self, func() {}, err
	}
	// Cross-build only from this project's source. An unrelated working
	// directory must never be mistaken for the 2o package.
	manifest, err := os.ReadFile("go.mod")
	if err != nil || !strings.HasPrefix(string(manifest), "module github.com/arthurnbr/2o\n") {
		return "", nil, errors.New("cross-architecture bootstrap requires Go and the 2o repository as the current directory")
	}
	dir, err := os.MkdirTemp("", "2o-cross-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	target := filepath.Join(dir, "2o")
	cmd := exec.Command("go", "build", "-o", target, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("cross-build Linux/%s: %w", arch, err)
	}
	return target, cleanup, nil
}

func sshSession(args []string) error {
	opts, host, command, e := splitSSH(args)
	if e != nil {
		return e
	}
	local, e := ensureDaemon()
	if e != nil {
		return e
	}
	remoteInfo, e := sshRun(opts, host, "uname -s; uname -m; id -u; if test -x \"$HOME/.local/bin/2o\"; then sha256sum \"$HOME/.local/bin/2o\" | cut -d ' ' -f 1; fi", nil)
	if e != nil {
		return fmt.Errorf("remote probe: %w", e)
	}
	lines := strings.Split(remoteInfo, "\n")
	if len(lines) < 3 {
		return fmt.Errorf("invalid remote probe: %q", remoteInfo)
	}
	arch := lines[1]
	if arch == "x86_64" {
		arch = "amd64"
	}
	if arch == "aarch64" {
		arch = "arm64"
	}
	if lines[0] != "Linux" {
		return fmt.Errorf("unsupported remote OS: %s", lines[0])
	}
	self, cleanup, e := remoteBinary(arch)
	if e != nil {
		return e
	}
	defer cleanup()
	f, e := os.Open(self)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	sum := sha256.New()
	if _, e = io.Copy(sum, f); e != nil {
		return e
	}
	installed := len(lines) > 3 && lines[3] == hex.EncodeToString(sum.Sum(nil))
	if !installed {
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			return e
		}
		script := "umask 077; mkdir -p \"$HOME/.local/bin\" && cat > \"$HOME/.local/bin/2o.tmp\" && chmod 700 \"$HOME/.local/bin/2o.tmp\" && mv \"$HOME/.local/bin/2o.tmp\" \"$HOME/.local/bin/2o\""
		if _, e = sshRun(opts, host, script, f); e != nil {
			return fmt.Errorf("bootstrap remote binary: %w", e)
		}
	}
	master, daemonAddr, e := readKey(keyPath(local))
	if e != nil {
		return fmt.Errorf("desktop daemon key: %w", e)
	}
	id, e := randomHex(12)
	if e != nil {
		return e
	}
	token, e := randomHex(32)
	if e != nil {
		return e
	}
	if e = controlRequest(local, master, wire.Message{Op: "register", Name: id, Hash: token}); e != nil {
		return fmt.Errorf("register session with desktop daemon: %w", e)
	}
	defer func() { _ = controlRequest(local, master, wire.Message{Op: "unregister", Name: id}) }()
	remoteDir := fmt.Sprintf("/tmp/2o-%s-%s", lines[2], id)
	var runErr error
	// The remote port is chosen on the server; another process may grab it
	// before sshd binds, which ExitOnForwardFailure reports as exit status 255.
	for attempt := range 3 {
		port, e := sshRun(opts, host, "\"$HOME/.local/bin/2o\" free-port", nil)
		if e != nil {
			return fmt.Errorf("pick remote port: %w", e)
		}
		remoteAddr := "127.0.0.1:" + port
		// The token travels on stdin so it never appears in a process list.
		setup := "umask 077; mkdir -m 700 " + quote(remoteDir) + " 2>/dev/null || test -d " + quote(remoteDir) + "; cat > " + quote(remoteDir+"/session") +
			" && if test ! -e \"$HOME/.local/bin/xdg-open\" && test ! -L \"$HOME/.local/bin/xdg-open\"; then ln -s 2o \"$HOME/.local/bin/xdg-open\"; fi"
		if _, e = sshRun(opts, host, setup, strings.NewReader(remoteAddr+" "+id+" "+token+"\n")); e != nil {
			return fmt.Errorf("create remote session: %w", e)
		}
		script := "TWO_O_SESSION=" + quote(remoteDir+"/session") + "; export TWO_O_SESSION; BROWSER=\"$HOME/.local/bin/2o\"; export BROWSER; PATH=\"$HOME/.local/bin:$PATH\"; export PATH; "
		script += "if ! \"$HOME/.local/bin/2o\" status >/dev/null; then printf '%s\\n' '2o: tunnel to the desktop is not working; the remote shell was not opened' >&2; exit 1; fi; "
		if len(command) == 0 {
			script += "exec \"$HOME/.local/bin/2o\" shell"
		} else {
			script += strings.Join(command, " ")
		}
		a := append(append([]string{}, opts...), "-o", "ExitOnForwardFailure=yes", "-R", remoteAddr+":"+daemonAddr)
		if len(command) == 0 {
			a = append(a, "-t")
		} else {
			a = append(a, "-T")
		}
		a = append(a, host, script)
		cmd := exec.Command("ssh", a...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		start := time.Now()
		runErr = cmd.Run()
		var exit *exec.ExitError
		// Only a fast 255 (forward refused before the shell starts) is retried.
		if attempt < 2 && errors.As(runErr, &exit) && exit.ExitCode() == 255 && time.Since(start) < 5*time.Second {
			continue
		}
		break
	}
	_, _ = sshRun(opts, host, "rm -rf -- "+quote(remoteDir), nil)
	return runErr
}
