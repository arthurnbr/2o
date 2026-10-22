package main

import (
	"errors"
	"fmt"
)

// version is set at build time: go build -ldflags "-X main.version=v0.3.0".
var version = "dev"

// userError is a failure explained for humans: a one-line message, optional
// hints ("→ …"), and the process exit code. main prints it with ui.Error/Hint.
type userError struct {
	msg   string
	hints []string
	code  int   // exit code; 0 means 1
	cause error // underlying error, for errors.Is/As; not printed
}

func (e *userError) Error() string { return e.msg }
func (e *userError) Unwrap() error { return e.cause }

// fail builds a userError: fail("no 2o tunnel in this shell").hint("connect with: 2o ssh HOST").
func fail(format string, a ...any) *userError {
	return &userError{msg: fmt.Sprintf(format, a...)}
}

func (e *userError) hint(format string, a ...any) *userError {
	e.hints = append(e.hints, fmt.Sprintf(format, a...))
	return e
}

func (e *userError) because(err error) *userError { e.cause = err; return e }

func (e *userError) exit(code int) *userError { e.code = code; return e }

// exitStatus is returned when a child (the remote command over ssh) already
// reported its own outcome: main exits with that code and prints nothing.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// exitCode maps any error to the process exit code.
func exitCode(err error) int {
	var es exitStatus
	if errors.As(err, &es) {
		return int(es)
	}
	var ue *userError
	if errors.As(err, &ue) && ue.code != 0 {
		return ue.code
	}
	return 1
}

// Exit codes (2o's own; a remote command's code passes through unchanged).
const (
	exitUsage    = 2   // bad command line
	exitNoTunnel = 3   // no reachable desktop
	exitRefused  = 4   // desktop refused (policy, confirmation)
	exitSSH      = 255 // ssh itself failed, same as ssh
)
