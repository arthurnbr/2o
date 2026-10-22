package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/arthurnbr/2o/internal/desktop"
)

// storeOnServer puts data where it can be pasted on this (remote) machine:
// the clipboard of its graphical session when there is one, found even though
// the SSH shell lacks WAYLAND_DISPLAY/DISPLAY; else the tmux paste buffer.
// It returns where the data went, for the caller's success line; errors are
// plain one-liners (matching desktop.ErrUnavailable when nothing could take
// the data) that the caller wraps with advice. Replaced in tests.
var storeOnServer = func(data []byte) (string, error) {
	s := desktop.FindSession()
	backend, gerr := desktop.CopyTo(s, data)
	if gerr == nil {
		return fmt.Sprintf("the server clipboard (%s, via %s)", backend, s.Source), nil
	}
	if os.Getenv("TMUX") == "" {
		return "", gerr
	}
	if err := tmuxLoadBuffer(data); err != nil {
		return "", fmt.Errorf("%v; tmux buffer: %v", gerr, err)
	}
	return "the tmux buffer (paste with prefix + ])", nil
}

// tmuxLoadBuffer stores data in a new tmux buffer. -w (tmux 3.2+) also
// forwards it to the attached terminal's clipboard with OSC 52 when tmux's
// set-clipboard option allows; older tmux rejects the flag, so retry without.
func tmuxLoadBuffer(data []byte) error {
	if tmux(data, "load-buffer", "-w", "-") == nil {
		return nil
	}
	return tmux(data, "load-buffer", "-")
}

func tmux(data []byte, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.Join(strings.Fields(string(out)), " "); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("tmux did not answer within 15s")
	}
	return err
}
