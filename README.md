# 2o (to open)

Open a file or URL on **your own desktop** from a remote SSH shell, with the file transferred over the existing SSH connection. Linux client and Linux server; `2o ssh` supports Linux amd64 and arm64 servers. A matching Linux client uploads its own binary; a different client architecture cross-builds from this checkout and needs Go 1.25+ locally. No Go or sudo on the server. Tailscale hostnames/IPs, SSH aliases, custom ports and ProxyJump use the normal `ssh` configuration.

## Build and connect

Run `2o ssh` on the computer where you want applications to open. That can itself be a host reached through SSH, if its desktop session is available; in that case files open **there**, not on the computer at the beginning of the SSH chain. An already-running SSH connection cannot be retrofitted with this forward.

```sh
git clone https://github.com/arthurnbr/2o.git
cd 2o
go build -o 2o .
./2o ssh omarchy                    # open on this desktop while working on omarchy
# or ./2o ssh 2serv-1               # SSH alias for 78.123.62.14:22
# or ./2o ssh arthur@78.123.62.14
```

If Go is absent on the SSH-origin desktop, compile **on a Linux machine with Go** for that desktop's architecture (`uname -m` gives `x86_64` → `amd64`, `aarch64` → `arm64`) and copy the binary back. Example with a Linux amd64 desktop and this checkout on `omarchy`:

```sh
# on omarchy, from the 2o checkout:
mkdir -p build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/2o-linux-amd64 .
# on your desktop, with SSH access to omarchy:
scp omarchy:~/Documents/2o/build/2o-linux-amd64 ~/Documents/2o/2o
chmod +x ~/Documents/2o/2o
~/Documents/2o/2o ssh omarchy
```

The binary belongs on the **SSH-origin desktop**; `2o ssh` installs the server copy automatically. If the remote Linux server has a different CPU architecture, cross-building its copy at connection time additionally requires Go and this source checkout on the SSH-origin desktop.

In the shell opened by `2o ssh`, run `2o status` to check the exact forwarded socket, then `2o open report.pdf`. To open on an earlier SSH hop instead, launch `2o ssh` from that earlier computer: each wrapper connection delivers to its immediate client desktop.

`2o ssh` launches a daemon in the desktop session if needed, verifies/installs the server binary, creates a private per-session Unix socket on the server, and adds an SSH remote Unix-socket forward. **Before presenting a remote shell it sends a ping through that exact socket**; a rejected or broken forward now fails immediately, with its path and OS error. Inside the shell `TWO_O_SOCK` and `BROWSER` are exported. No TCP listener, no SFTP login; SSH must permit remote Unix-socket forwarding (OpenSSH `AllowStreamLocalForwarding`, or the corresponding Tailscale SSH policy). The daemon uses `$XDG_RUNTIME_DIR/2o.sock` or a private temp directory if unset. You can run `./2o daemon` from your desktop session explicitly.

With a normal SSH connection rather than `2o ssh`, create the forward manually and export `TWO_O_SOCK` on the remote side. `2o` can discover live private `/tmp/2o-UID-*/sock` session sockets when a tmux session has a stale `TWO_O_SOCK`. A per-session path avoids collisions across simultaneous connections. Where multiple sessions exist, explicitly setting `TWO_O_SOCK` selects the intended desktop.

## Remote commands

```sh
2o open https://example.com
2o open report.pdf                   # private local cached copy, not a server file:// URL
2o open ./folder                     # tar stream, safely extracted locally
printf 'hello' | 2o copy             # remote stdin -> local clipboard
2o paste > local-clipboard.txt       # local clipboard -> remote stdout
2o edit report.txt                   # waits for local editor; writes back unless conflict
2o open http://localhost:3000/page   # local browser -> remote HTTP port via SSH
2o notify 'Build' 'Finished'
```

`2o open http://localhost:PORT` runs a loopback-only HTTP bridge and **stays running** while it serves the opened page; Ctrl-C closes the bridge. URLs on remote loopback are never rewritten into your local `localhost`. Plain HTTP only for localhost; non-local HTTPS URLs open normally. HTTP applications may create absolute URLs/websocket connections that need their own bridge.

`BROWSER` is set automatically inside `2o ssh`. The wrapper also creates `~/.local/bin/xdg-open -> 2o` on the server **if no file already exists at that path**, and prepends that directory to remote `PATH`. It never replaces an existing executable. Do not shadow `xdg-open` on the desktop: it is how the local daemon launches applications.

For editing, set a *blocking graphical editor* on your **desktop** before the daemon starts, e.g. `export TWO_O_EDITOR='code --wait'` or `TWO_O_EDITOR='gvim -f'`. `VISUAL` and `EDITOR` are also accepted; otherwise a detected graphical editor is used. The editor must exit after the edit. The remote file is atomically replaced after an optimistic hash check. On conflict, the command reports the full path of the retained **local recovery copy**; the remote file is not overwritten. As with other optimistic checks, a writer racing between the final hash check and rename is not excluded. `2o open` creates only a copy; it does not sync edits back.

## Limits and trust

- Max transferred file/archive and clipboard: 256 MiB. OSC 52 fallback for `2o copy` without a tunnel is capped at 100 KiB; the terminal must support OSC 52.
- Files arrive under `~/.cache/2o` (or `$XDG_CACHE_HOME/2o`). The cache currently needs manual cleanup. A remote account with access to the SSH session socket can request permitted desktop actions. Do not use an untrusted remote account.
- Executable files, `.desktop` files, scripts and symlinks are refused for opening. A received path is **never** treated as a local path. The local daemon listens on a user-only Unix socket, not the network.
- `2o edit` requires a graphical blocking editor on the desktop; headless desktop integrations fail rather than silently opening on the server.
- SSH servers with Unix-socket forwarding disabled cannot use this transport. Remote HTTPS localhost and WebSockets are not currently bridged.

See [PLAN.md](PLAN.md) for architecture, security review and milestone acceptance.
