package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/files"
	"github.com/arthurnbr/2o/internal/ui"
	"github.com/arthurnbr/2o/internal/wire"
)

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	var es exitStatus
	if !errors.As(err, &es) {
		var ue *userError
		if errors.As(err, &ue) {
			ui.Error("%s", ue.msg)
			for _, h := range ue.hints {
				ui.Hint("%s", h)
			}
		} else {
			ui.Error("%s", err)
		}
	}
	os.Exit(exitCode(err))
}

// command is one entry of `2o --help`; hidden ones are internal plumbing.
type command struct {
	name, args, summary, details string
	hidden                       bool
}

var commands = []command{
	{name: "ssh", args: "[ssh options] HOST [command…]", summary: "connect to HOST with a tunnel back to this desktop",
		details: "Runs ssh with your usual options. The remote shell (or command) gets 2o on its PATH and a\n" +
			"private tunnel to this desktop; it closes with the session. The first connection installs 2o\n" +
			"in ~/.local/bin on HOST. Exit status is the remote command's, or 255 when ssh fails."},
	{name: "open", args: "FILE|DIR|URL", summary: "open on the desktop (2o FILE works too)",
		details: "URLs open in the desktop browser; http://localhost:PORT is bridged so the desktop browser\n" +
			"reaches the server's port until Ctrl-C. Files and folders are copied to the desktop cache\n" +
			"(~/.cache/2o) and opened there: changes are not sent back (use 2o edit)."},
	{name: "edit", args: "[-n] FILE[:LINE]", summary: "edit in the desktop's editor, saved back here",
		details: "Opens a copy in the desktop's graphical editor (TWO_O_EDITOR on the desktop). Save and close\n" +
			"the editor to write the file back; if the file changed here meanwhile, nothing is overwritten.\n" +
			"Whenever the result cannot be written back, the desktop keeps your edits and 2o prints where.\n" +
			"  -n, --new   create FILE if it does not exist\n" +
			"  FILE:LINE   open at LINE (code, codium, zed, subl, kate, gvim, gedit, emacs)"},
	{name: "copy", args: "< DATA", summary: "put stdin on the desktop clipboard",
		details: "Reads stdin, e.g. `git diff | 2o copy`. Without a tunnel, falls back to the terminal\n" +
			"clipboard (OSC 52) when stdout is a terminal. Silent on success."},
	{name: "paste", args: "[-c]", summary: "print the desktop clipboard",
		details: "Writes the desktop clipboard to stdout, e.g. `2o paste > notes.txt`.\n" +
			"  -c, --clipboard   put it in this server's clipboard (or tmux buffer) instead"},
	{name: "notify", args: "TITLE BODY", summary: "show a desktop notification",
		details: "e.g. `make; 2o notify build \"done: $?\"`. Silent on success."},
	{name: "status", args: "[-v]", summary: "show which desktop this shell reaches",
		details: "Prints the desktop, its 2o version and the session age. Exit 3 when there is no tunnel.\n" +
			"  -v, --verbose   same as 2o doctor"},
	{name: "doctor", summary: "check the tunnel and the desktop's tools",
		details: "Checks this shell's session, the tunnel, versions, and on the desktop: graphical session,\n" +
			"opener, clipboard, notifications, editor, transfer policy and log. Changes nothing."},
	// Internal: started by 2o ssh, not shown in help.
	{name: "daemon", summary: "run the desktop daemon (2o ssh starts it)", hidden: true},
	{name: "shell", summary: "start a login shell (used by 2o ssh)", hidden: true},
}

func lookup(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func isHelp(a string) bool { return a == "-h" || a == "--help" }

// helpPainter colours help on stdout only when stdout is a terminal.
func helpPainter() func(code, s string) string {
	on := ui.IsTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return func(code, s string) string {
		if !on {
			return s
		}
		return code + s + "\x1b[0m"
	}
}

const (
	hBold = "\x1b[1m"
	hCyan = "\x1b[36m"
	hDim  = "\x1b[2m"
)

func usage(w io.Writer) {
	p := helpPainter()
	fmt.Fprintf(w, "%s — open things from an SSH server on your desktop\n\n", p(hBold, "2o"))
	fmt.Fprintf(w, "%s\n", p(hBold, "Usage"))
	for _, c := range commands {
		if c.hidden {
			continue
		}
		left := c.name
		if c.args != "" {
			left += " " + c.args
		}
		fmt.Fprintf(w, "  2o %s%s  %s\n", p(hCyan, left), strings.Repeat(" ", max(0, 36-utf8.RuneCountInString(left))), c.summary)
	}
	fmt.Fprintf(w, "\n%s\n", p(hBold, "Examples"))
	for _, e := range [][2]string{
		{"2o ssh myserver", "on the desktop: log in with a tunnel"},
		{"2o report.pdf", "on the server: open it on the desktop"},
		{"2o edit app.go:42", "edit at line 42 in the desktop editor"},
		{"git diff | 2o copy", "copy to the desktop clipboard"},
		{"2o open http://localhost:3000", "see a server dev site in the desktop browser"},
	} {
		fmt.Fprintf(w, "  %s%s  %s\n", e[0], strings.Repeat(" ", max(0, 30-len(e[0]))), p(hDim, e[1]))
	}
	fmt.Fprintf(w, "\nRun %s for details, %s for the version.\n", p(hCyan, "2o COMMAND --help"), p(hCyan, "2o --version"))
	fmt.Fprintf(w, "Exit codes: 0 ok, 1 failed, 2 usage, 3 no tunnel to the desktop, 4 refused by the desktop.\n")
}

func commandUsage(w io.Writer, c command) {
	p := helpPainter()
	line := "2o " + c.name
	if c.args != "" {
		line += " " + c.args
	}
	fmt.Fprintf(w, "%s %s\n\n%s.\n", p(hBold, "Usage:"), p(hCyan, line), strings.ToUpper(c.summary[:1])+c.summary[1:])
	if c.details != "" {
		fmt.Fprintf(w, "\n%s\n", c.details)
	}
}

func usageError(name, format string, a ...any) error {
	c, _ := lookup(name)
	e := fail(format, a...)
	if c.args != "" {
		e = e.hint("usage: 2o %s %s", c.name, c.args)
	} else {
		e = e.hint("usage: 2o %s", c.name)
	}
	return e.hint("run 2o %s --help for details", name).exit(exitUsage)
}

// distance is the Levenshtein edit distance, for "did you mean".
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func unknownCommand(name string) error {
	e := fail("unknown command %q", name)
	best, bestD := "", 3
	for _, c := range commands {
		if d := distance(name, c.name); !c.hidden && d < bestD {
			best, bestD = c.name, d
		}
	}
	if best != "" {
		e = e.hint("did you mean: 2o %s", best)
	}
	return e.hint("run 2o --help to see the commands").exit(exitUsage)
}

// openable reports whether a lone argument is meant to be opened (`2o
// report.pdf`): an existing path or a URL. Anything else is a mistyped command.
func openable(arg string) bool {
	if strings.Contains(arg, "://") {
		return true
	}
	_, err := os.Lstat(arg)
	return err == nil
}

func run(args []string) error {
	if base := filepath.Base(os.Args[0]); base == "xdg-open" || base == "open" {
		if len(args) == 1 && isHelp(args[0]) {
			fmt.Printf("Usage: %s FILE|DIR|URL\n\nOpens it on the desktop through 2o.\n", base)
			return nil
		}
		if len(args) != 1 {
			return fail("%s takes exactly one FILE, DIR or URL", base).exit(exitUsage)
		}
		return open(args[0])
	}
	if len(args) == 0 || args[0] == "help" || isHelp(args[0]) {
		if len(args) == 2 && args[0] == "help" {
			if c, ok := lookup(args[1]); ok {
				commandUsage(os.Stdout, c)
				return nil
			}
			return unknownCommand(args[1])
		}
		usage(os.Stdout)
		return nil
	}
	if args[0] == "--version" {
		fmt.Printf("2o %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	name, rest := args[0], args[1:]
	c, known := lookup(name)
	if !known {
		if len(args) == 1 && !strings.HasPrefix(name, "-") && openable(name) {
			return open(name)
		}
		if strings.HasPrefix(name, "-") {
			return fail("unknown option %q", name).hint("run 2o --help to see the commands").exit(exitUsage)
		}
		return unknownCommand(name)
	}
	// `2o ssh HOST cmd --help` and `2o notify T --help` pass --help on.
	if len(rest) > 0 && isHelp(rest[0]) {
		commandUsage(os.Stdout, c)
		return nil
	}
	if name != "ssh" && name != "notify" {
		for _, a := range rest {
			if isHelp(a) {
				commandUsage(os.Stdout, c)
				return nil
			}
		}
	}
	switch name {
	case "daemon":
		if len(rest) != 0 {
			return usageError(name, "2o daemon takes no arguments")
		}
		return daemon()
	case "ssh":
		return sshSession(rest)
	case "shell":
		if len(rest) != 0 {
			return usageError(name, "2o shell takes no arguments")
		}
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		return syscall.Exec(shell, []string{shell, "-l"}, os.Environ())
	case "open":
		if len(rest) != 1 {
			return usageError(name, "2o open takes exactly one FILE, DIR or URL")
		}
		return open(rest[0])
	case "copy":
		if len(rest) != 0 {
			return usageError(name, "2o copy takes no arguments; it reads stdin")
		}
		return copyStdin()
	case "paste":
		toServer := false
		for _, a := range rest {
			if a != "-c" && a != "--clipboard" {
				return usageError(name, "unknown option %q for 2o paste", a)
			}
			toServer = true
		}
		return paste(toServer)
	case "notify":
		if len(rest) != 2 {
			return usageError(name, "2o notify takes a TITLE and a BODY")
		}
		return send(wire.Message{Op: "notify", Title: rest[0], Body: rest[1]})
	case "edit":
		create, target := false, ""
		for i, a := range rest {
			switch {
			case a == "--" && i == len(rest)-2:
				target = rest[i+1]
			case a == "--":
				return usageError(name, "2o edit takes exactly one FILE")
			case a == "-n" || a == "--new":
				create = true
				continue
			case strings.HasPrefix(a, "-") && target == "":
				return usageError(name, "unknown option %q for 2o edit", a)
			case target != "":
				return usageError(name, "2o edit takes exactly one FILE")
			default:
				target = a
				continue
			}
			break
		}
		if target == "" {
			return usageError(name, "2o edit needs a FILE")
		}
		path, line := editTarget(target)
		return editFile(path, line, create)
	case "status":
		verbose := false
		for _, a := range rest {
			if a != "-v" && a != "--verbose" {
				return usageError(name, "unknown option %q for 2o status", a)
			}
			verbose = true
		}
		if verbose {
			return doctor()
		}
		return status()
	case "doctor":
		if len(rest) != 0 {
			return usageError(name, "2o doctor takes no arguments")
		}
		return doctor()
	}
	return unknownCommand(name)
}

// maxCopyBytes bounds what copy reads from stdin; the desktop applies its
// own, usually smaller, TWO_O_CLIPBOARD_BYTES.
const maxCopyBytes = 1 << 30

func copyStdin() error {
	if ui.IsTerminal(os.Stdin) {
		ui.Info("type the text to copy, then press Ctrl-D (or pipe into 2o copy)")
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxCopyBytes+1))
	if err != nil {
		return fail("could not read stdin").because(err).hint("%v", err)
	}
	if len(b) > maxCopyBytes {
		return fail("stdin is larger than %s, too much for a clipboard", humanSize(maxCopyBytes)).
			hint("send it as a file instead: 2o open FILE").exit(exitUsage)
	}
	c, err := request(wire.Message{Op: "copy", Size: int64(len(b))})
	if err != nil {
		var ue *userError
		if !errors.As(err, &ue) || ue.code != exitNoTunnel {
			return err
		}
		if !ui.IsTerminal(os.Stdout) {
			return ue.hint("without a tunnel, 2o copy can use the terminal clipboard (OSC 52) only when its output is a terminal")
		}
		if err = desktop.OSC52(b, os.Stdout); err != nil {
			if errors.Is(err, desktop.ErrTooLarge) {
				return ue.hint("the terminal clipboard (OSC 52) takes at most 100 KiB; this is %s", humanSize(int64(len(b))))
			}
			return ue.hint("the terminal clipboard (OSC 52) failed too: %v", err)
		}
		ui.Info("no desktop tunnel; copied %s through the terminal (OSC 52) — works if your terminal allows it", humanSize(int64(len(b))))
		return nil
	}
	defer c.Close()
	if _, err = c.Write(b); err != nil {
		// A refusal (e.g. over the desktop's limit) may already be waiting.
		if rerr := response(c); rerr != nil {
			return rerr
		}
		return lostDesktop("the desktop", "sending the clipboard", err)
	}
	return response(c)
}

func paste(toServer bool) error {
	c, err := request(wire.Message{Op: "paste"})
	if err != nil {
		return err
	}
	defer c.Close()
	m, err := wire.Read(c)
	if err != nil {
		return lostDesktop("the desktop", "reading its clipboard", err)
	}
	if err = remoteErr(m); err != nil {
		return err
	}
	if m.Size < 0 || m.Size > files.MaxBytes {
		return fail("the desktop announced an invalid clipboard size (%d bytes)", m.Size)
	}
	if !toServer {
		if _, err = io.CopyN(os.Stdout, c, m.Size); err != nil {
			return fail("could not write the clipboard to stdout").because(err).hint("%v", err)
		}
		return nil
	}
	b := make([]byte, m.Size)
	if _, err = io.ReadFull(c, b); err != nil {
		return lostDesktop("the desktop", "reading its clipboard", err)
	}
	where, err := storeOnServer(b)
	if err != nil {
		return fail("could not store the clipboard on this server").because(err).hint("%v", err).
			hint("use '2o paste > FILE' or '2o paste | CMD'")
	}
	ui.Success("%s copied to %s", humanSize(m.Size), where)
	return nil
}

// ago formats how long ago t was, coarsely: "3 min", "2 h 5 min", "4 days".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func ping() (wire.Message, error) {
	c, err := request(wire.Message{Op: "ping"})
	if err != nil {
		return wire.Message{}, err
	}
	defer c.Close()
	m, err := wire.Read(c)
	if err != nil {
		return m, lostDesktop("the desktop", "checking the tunnel", err)
	}
	return m, remoteErr(m)
}

func status() error {
	m, err := ping()
	if err != nil {
		return err
	}
	where := m.Desktop
	if where == "" {
		where = targetDesktop()
	}
	ui.Success("connected to %s", ui.Bold(where))
	rows := [][2]string{{"desktop", where}, {"version", versionLine(m.Version)}}
	if s, err := currentSession(); err == nil {
		rows = append(rows, [2]string{"session", fmt.Sprintf("%s old (%s)", ago(time.Since(modTime(s.path))), s.path)})
	}
	if !ui.Quiet() {
		ui.Table(rows)
	}
	if m.Version != "" && m.Version != version {
		ui.Warn("the desktop runs 2o %s, this server %s", m.Version, version)
		ui.Hint("reconnect with 2o ssh from the desktop to update the server copy")
	}
	return nil
}

func versionLine(remote string) string {
	if remote == "" {
		remote = "unknown (older than this 2o)"
	}
	if remote == version {
		return remote
	}
	return fmt.Sprintf("%s on the desktop, %s here", remote, version)
}

// doctor checks this end (session, tunnel, versions) and asks the desktop to
// check itself. On the desktop itself it runs the desktop checks directly.
func doctor() error {
	problems := 0
	check := func(ok bool, name, detail string) {
		ui.Check(ok, name, detail)
		if !ok {
			problems++
		}
	}
	ui.Header("2o doctor")
	self, _ := os.Executable()
	check(true, "this 2o", fmt.Sprintf("%s %s/%s %s", version, runtime.GOOS, runtime.GOARCH, self))
	if !remote() {
		sock, err := socketPath()
		if err == nil {
			var c net.Conn
			if c, err = net.DialTimeout("unix", sock, time.Second); err == nil {
				c.Close()
			}
		}
		if err != nil {
			check(false, "desktop daemon", "not running (2o ssh starts it)")
		} else {
			check(true, "desktop daemon", "running ("+sock+")")
		}
		for _, r := range doctorChecks() {
			check(r.OK, r.Name, r.Detail)
		}
		return doctorResult(problems)
	}
	s, err := currentSession()
	if err != nil {
		check(false, "session", errorLine(err))
		return doctorResult(problems)
	}
	check(true, "session", fmt.Sprintf("%s, %s old", s.path, ago(time.Since(modTime(s.path)))))
	c, err := request(wire.Message{Op: "doctor"})
	if err != nil {
		check(false, "tunnel", errorLine(err))
		return doctorResult(problems)
	}
	defer c.Close()
	m, err := wire.Read(c)
	if err != nil {
		check(false, "tunnel", "the desktop closed the connection")
		return doctorResult(problems)
	}
	check(true, "tunnel", "connected to "+firstNonEmpty(m.Desktop, s.desktop, "the desktop"))
	if m.Error != "" {
		// A daemon older than `doctor`: the tunnel works, the report does not.
		check(false, "desktop 2o", "too old to run checks; reconnect with 2o ssh to restart it")
		return doctorResult(problems)
	}
	check(m.Version == version, "desktop 2o", versionLine(m.Version))
	for _, r := range m.Checks {
		check(r.OK, r.Name, r.Detail)
	}
	return doctorResult(problems)
}

func doctorResult(problems int) error {
	if problems == 0 {
		return nil
	}
	s := ""
	if problems > 1 {
		s = "s"
	}
	return fail("%d problem%s found", problems, s).hint("fix the ✗ lines above, then run 2o doctor again")
}

func errorLine(err error) string {
	var ue *userError
	if errors.As(err, &ue) {
		return strings.Join(append([]string{ue.msg}, ue.hints...), "; ")
	}
	return err.Error()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
