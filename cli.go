package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

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

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: 2o daemon|ssh HOST|shell|open URL|FILE|copy|paste|edit FILE|notify TITLE BODY")
	}
	if filepath.Base(os.Args[0]) == "xdg-open" || filepath.Base(os.Args[0]) == "open" {
		if len(args) != 1 {
			return errors.New("usage: xdg-open URL|FILE")
		}
		return open(args[0])
	}
	switch args[0] {
	case "daemon":
		return daemon()
	case "ssh":
		return sshSession(args[1:])
	case "free-port":
		port, err := freePort()
		if err != nil {
			return err
		}
		fmt.Println(port)
		return nil
	case "status":
		if len(args) != 1 {
			return errors.New("usage: 2o status")
		}
		c, err := request(wire.Message{Op: "ping"})
		if err != nil {
			return err
		}
		defer c.Close()
		if err = response(c); err != nil {
			return err
		}
		fmt.Println("2o: desktop tunnel active")
		return nil
	case "shell":
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		return syscall.Exec(shell, []string{shell, "-l"}, os.Environ())
	case "open":
		if len(args) != 2 {
			return errors.New("usage: 2o open URL|FILE")
		}
		return open(args[1])
	case "copy":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, defaultClipBytes*64+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > defaultClipBytes*64 {
			return errors.New("clipboard input too large")
		}
		c, err := request(wire.Message{Op: "copy", Size: int64(len(b))})
		if err != nil {
			if os.Getenv("SSH_CONNECTION") != "" {
				return desktop.OSC52(b, os.Stdout)
			}
			return err
		}
		defer c.Close()
		if _, err = c.Write(b); err != nil {
			return err
		}
		return response(c)
	case "paste":
		toClipboard := len(args) == 2 && (args[1] == "-c" || args[1] == "--clipboard")
		if len(args) > 2 || (len(args) == 2 && !toClipboard) {
			return errors.New("usage: 2o paste [-c|--clipboard]")
		}
		c, err := request(wire.Message{Op: "paste"})
		if err != nil {
			return err
		}
		defer c.Close()
		m, err := wire.Read(c)
		if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.Size < 0 || m.Size > files.MaxBytes {
			return errors.New("clipboard too large")
		}
		if !toClipboard {
			_, err = io.CopyN(os.Stdout, c, m.Size)
			return err
		}
		b := make([]byte, m.Size)
		if _, err = io.ReadFull(c, b); err != nil {
			return err
		}
		where, err := storeOnServer(b)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "2o: %s copied to %s\n", humanSize(m.Size), where)
		return nil
	case "notify":
		if len(args) != 3 {
			return errors.New("usage: 2o notify TITLE BODY")
		}
		return send(wire.Message{Op: "notify", Title: args[1], Body: args[2]})
	case "edit":
		if len(args) != 2 {
			return errors.New("usage: 2o edit FILE")
		}
		return editFile(args[1])
	default:
		if len(args) == 1 {
			return open(args[0])
		}
		return errors.New("unknown command")
	}
}
