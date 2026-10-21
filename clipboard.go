package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/arthurnbr/2o/internal/desktop"
)

// storeOnServer puts data where it can be pasted on this (remote) machine:
// the clipboard of its graphical session when there is one, found even though
// the SSH shell lacks WAYLAND_DISPLAY/DISPLAY; else the tmux paste buffer.
// Replaced in tests.
var storeOnServer = func(data []byte) (string, error) {
	s := desktop.FindSession()
	backend, gerr := desktop.CopyTo(s, data)
	if gerr == nil {
		return fmt.Sprintf("the server clipboard (%s, via %s)", backend, s.Source), nil
	}
	if os.Getenv("TMUX") != "" {
		if err := tmuxLoadBuffer(data); err != nil {
			return "", fmt.Errorf("graphical clipboard: %v; tmux: %w", gerr, err)
		}
		return "the tmux buffer (paste with prefix + ])", nil
	}
	return "", errors.Join(gerr, errors.New("not inside tmux either; use '2o paste > FILE' or '2o paste | CMD'"))
}

func tmuxLoadBuffer(data []byte) error {
	cmd := exec.Command("tmux", "load-buffer", "-")
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
