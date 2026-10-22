// Package ui prints human progress and diagnostics on stderr. stdout is never
// touched, so `2o paste > file` and pipes stay clean.
//
// Output adapts to where stderr goes:
//   - a terminal: colours, and steps that redraw in place (spinner → ✓/✗);
//   - a pipe/file/CI: plain lines, no escape codes, no redraw;
//   - quiet (TWO_O_QUIET=1 or SetQuiet): only warnings and errors.
//
// NO_COLOR (https://no-color.org) disables colours; TERM=dumb disables both
// colours and redraw.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	mu     sync.Mutex
	out    io.Writer = os.Stderr
	tty              = isTTY(os.Stderr)
	colour           = tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	redraw           = tty && os.Getenv("TERM") != "dumb"
	quiet            = os.Getenv("TWO_O_QUIET") != ""
)

// SetOutput redirects UI output; for tests. It disables colour and redraw.
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	out, tty, colour, redraw = w, false, false, false
}

// SetQuiet hides info and step lines; warnings and errors still show.
func SetQuiet(q bool) { mu.Lock(); quiet = q; mu.Unlock() }

// Quiet reports whether info lines are hidden (TWO_O_QUIET or SetQuiet).
func Quiet() bool { mu.Lock(); defer mu.Unlock(); return quiet }

// Interactive reports whether stderr is a terminal (prompts, spinners).
func Interactive() bool { return tty }

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	blue   = "\x1b[34m"
	cyan   = "\x1b[36m"
)

func paint(code, s string) string {
	if !colour || s == "" {
		return s
	}
	return code + s + reset
}

// Bold, Dim, Accent style inline fragments, e.g. ui.Info("connected to %s", ui.Bold(host)).
func Bold(s string) string   { return paint(bold, s) }
func Dim(s string) string    { return paint(dim, s) }
func Accent(s string) string { return paint(cyan, s) }

func line(sym, code, format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, "%s %s\n", paint(code, sym), fmt.Sprintf(format, a...))
}

// Info: neutral progress line ("• connected to omarchy").
func Info(format string, a ...any) {
	if quiet {
		return
	}
	line("•", blue, format, a...)
}

// Success: final positive outcome ("✓ desktop tunnel ready").
func Success(format string, a ...any) {
	if quiet {
		return
	}
	line("✓", green, format, a...)
}

// Note: a dim, symbol-less aside ("tunnel closed"); hidden when quiet.
func Note(format string, a ...any) {
	if quiet {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(out, paint(dim, fmt.Sprintf(format, a...)))
}

// Warn: something the user should know but that did not fail.
func Warn(format string, a ...any) { line("!", yellow, format, a...) }

// Error: a failure, printed by main before exiting. Hint lines follow.
func Error(format string, a ...any) { line("✗", red+bold, format, a...) }

// Hint: an indented suggestion under an error or warning ("→ run 2o doctor").
func Hint(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, "  %s %s\n", paint(dim, "→"), paint(dim, fmt.Sprintf(format, a...)))
}

// Step is a running phase. On a terminal it shows a spinner, then is
// replaced by its outcome; elsewhere it prints one line when it ends.
type Step struct {
	label string
	start time.Time
	stop  chan struct{}
	done  chan struct{}
	ended bool
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Start begins a step: s := ui.Start("installing 2o on %s", host); … s.Done().
func Start(format string, a ...any) *Step {
	s := &Step{label: fmt.Sprintf(format, a...), start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	mu.Lock()
	live := redraw && !quiet
	mu.Unlock()
	if !live {
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			mu.Lock()
			fmt.Fprintf(out, "\r\x1b[2K%s %s", paint(cyan, spinFrames[i%len(spinFrames)]), s.label)
			mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
	return s
}

// Begin starts a step that does not animate: on a terminal its label stays
// on the current line, so a child process can prompt there (ssh asking for a
// password) without a spinner erasing the prompt. It ends like any Step.
func Begin(format string, a ...any) *Step {
	s := &Step{label: fmt.Sprintf(format, a...), start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)
	mu.Lock()
	defer mu.Unlock()
	if redraw && !quiet {
		fmt.Fprintf(out, "%s %s… ", paint(cyan, "•"), s.label)
	}
	return s
}

func (s *Step) finish(sym, code, label string, alwaysShow bool) {
	if s.ended {
		return
	}
	s.ended = true
	close(s.stop)
	<-s.done
	mu.Lock()
	defer mu.Unlock()
	if redraw && !quiet {
		fmt.Fprint(out, "\r\x1b[2K")
	}
	if quiet && !alwaysShow {
		return
	}
	el := time.Since(s.start)
	took := ""
	if el >= 700*time.Millisecond {
		took = " " + paint(dim, fmt.Sprintf("(%.1fs)", el.Seconds()))
	}
	fmt.Fprintf(out, "%s %s%s\n", paint(code, sym), label, took)
}

// Done ends the step successfully, keeping its label.
func (s *Step) Done() { s.finish("✓", green, s.label, false) }

// DoneAs ends the step successfully with a different label ("2o already up to date").
func (s *Step) DoneAs(format string, a ...any) {
	s.finish("✓", green, fmt.Sprintf(format, a...), false)
}

// Skip ends the step as not needed, dimmed.
func (s *Step) Skip(format string, a ...any) {
	s.finish("–", dim, paint(dim, fmt.Sprintf(format, a...)), false)
}

// Fail ends the step as failed; it is always shown, even when quiet.
func (s *Step) Fail() { s.finish("✗", red+bold, s.label, true) }

// End finishes the step from an error: Done on nil, Fail otherwise. Returns err.
func (s *Step) End(err error) error {
	if err != nil {
		s.Fail()
	} else {
		s.Done()
	}
	return err
}

// Update changes a running step's label, e.g. to show transfer progress.
// Only a live (terminal) step shows it; elsewhere the final line is printed.
func (s *Step) Update(format string, a ...any) {
	mu.Lock()
	s.label = fmt.Sprintf(format, a...)
	mu.Unlock()
}

// Warn prints a warning while the step keeps running: on a terminal it goes
// above the spinner, which redraws on its next tick.
func (s *Step) Warn(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if redraw && !quiet && !s.ended {
		fmt.Fprint(out, "\r\x1b[2K")
	}
	fmt.Fprintf(out, "%s %s\n", paint(yellow, "!"), fmt.Sprintf(format, a...))
}

// Live reports whether steps redraw in place (a terminal, not quiet), so
// callers can skip work that only feeds Update.
func Live() bool {
	mu.Lock()
	defer mu.Unlock()
	return redraw && !quiet
}

// IsTerminal reports whether f is a terminal, e.g. before writing escape
// sequences to stdout.
func IsTerminal(f *os.File) bool { return isTTY(f) }

// Header prints a short title line, e.g. "2o → omarchy".
func Header(format string, a ...any) {
	if quiet {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(out, paint(bold, fmt.Sprintf(format, a...)))
}

// Prompt writes a question without a newline, for a Y/n answer typed on the
// same line. It is shown even when quiet: the user must see what is asked.
func Prompt(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, "%s %s", paint(yellow, "?"), fmt.Sprintf(format, a...))
}

// Table prints aligned "key  value" rows (used by doctor/status -v).
func Table(rows [][2]string) {
	w := 0
	for _, r := range rows {
		w = max(w, len(r[0]))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range rows {
		fmt.Fprintf(out, "  %s  %s\n", paint(dim, r[0]+strings.Repeat(" ", w-len(r[0]))), r[1])
	}
}

// Check prints one doctor line: ok → ✓, else ✗ with the problem.
func Check(ok bool, label, detail string) {
	sym, code := "✓", green
	if !ok {
		sym, code = "✗", red
	}
	mu.Lock()
	defer mu.Unlock()
	if detail != "" {
		detail = "  " + paint(dim, detail)
	}
	fmt.Fprintf(out, "  %s %s%s\n", paint(code, sym), label, detail)
}

func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
