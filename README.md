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

In the shell opened by `2o ssh`, run `2o status` to check the tunnel, then `2o open report.pdf`. To open on an earlier SSH hop instead, launch `2o ssh` from that earlier computer: each wrapper connection delivers to its immediate client desktop.

`2o ssh` launches a daemon in the desktop session if needed, verifies/installs the server binary, registers a fresh per-session secret with the daemon, and adds an SSH **remote TCP forward** from a free loopback port on the server (`127.0.0.1` only) to the daemon's loopback listener. The secret is written over SSH stdin into a private `/tmp/2o-UID-ID/session` file (mode 0600, directory 0700) and exported as `TWO_O_SESSION`, together with `BROWSER`. **Before presenting a remote shell it checks the tunnel end to end**; a broken forward fails immediately.

Why TCP rather than a forwarded Unix socket: Tailscale SSH creates reverse Unix sockets as root with mode 0600, so the logged-in user gets `permission denied`. A loopback port is reachable by any local user of the server, so every connection runs a mutual HMAC challenge with the session secret: the daemon must prove the secret *before* the client sends anything (an impostor on a stale port receives nothing), and the client must prove it before the daemon acts. Closing the session unregisters its secret. No SFTP login. The daemon listens on `$XDG_RUNTIME_DIR/2o-v2.sock` (control) and a random `127.0.0.1` port; you can run `./2o daemon` from your desktop session explicitly.

In a reattached tmux session whose `TWO_O_SESSION` is stale, `2o` tries the newest other live private session file of the same user.

## Remote commands

```sh
2o open https://example.com
2o open report.pdf                   # private local cached copy, not a server file:// URL
2o open ./folder                     # tar stream, safely extracted locally
printf 'hello' | 2o copy             # remote stdin -> desktop clipboard
2o paste > local-clipboard.txt       # desktop clipboard -> remote stdout (file, pipe, terminal)
2o paste -c                          # desktop clipboard -> the server's own clipboard
2o edit report.txt                   # waits for local editor; writes back unless conflict
2o open http://localhost:3000/page   # local browser -> remote HTTP port via SSH
2o notify 'Build' 'Finished'
```

`2o open http://localhost:PORT` runs a loopback-only HTTP bridge and **stays running** while it serves the opened page; Ctrl-C closes the bridge. URLs on remote loopback are never rewritten into your local `localhost`. Plain HTTP only for localhost; non-local HTTPS URLs open normally. HTTP applications may create absolute URLs/websocket connections that need their own bridge.

`BROWSER` is set automatically inside `2o ssh`. The wrapper also creates `~/.local/bin/xdg-open -> 2o` on the server **if no file already exists at that path**, and prepends that directory to remote `PATH`. It never replaces an existing executable. Do not shadow `xdg-open` on the desktop: it is how the local daemon launches applications.

For editing, set a *blocking graphical editor* on your **desktop** before the daemon starts, e.g. `export TWO_O_EDITOR='code --wait'` or `TWO_O_EDITOR='gvim -f'`. `VISUAL` and `EDITOR` are also accepted; otherwise a detected graphical editor is used. The editor must exit after the edit. The remote file is atomically replaced after an optimistic hash check. On conflict, the command reports the full path of the retained **local recovery copy**; the remote file is not overwritten. As with other optimistic checks, a writer racing between the final hash check and rename is not excluded. `2o open` creates only a copy; it does not sync edits back.

### Clipboard on any desktop

The same clipboard logic runs on both ends (the desktop daemon for `copy`/`paste`, the server for `paste -c`). It does not depend on which desktop you use: Plasma, GNOME, Hyprland, Sway, Xfce… all run on Wayland or X11, and what differs is how they let a window-less program set the clipboard.

1. **Find the graphical session**, even from an SSH shell or a daemon started by systemd/autostart where `WAYLAND_DISPLAY`/`DISPLAY` are empty: current environment → `systemctl --user show-environment` (where Plasma, GNOME, Hyprland… publish their session) → the user's `wayland-*` and X11 sockets.
2. **Try the tools available for that session**, in order: `wl-copy` (Wayland), Klipper over D-Bus (Plasma, text only), `xclip` then `xsel` (X11, and XWayland on GNOME, whose clipboard is synced with Wayland), `pbcopy` on macOS.
3. **Read back** after each copy. Some compositors (GNOME/Mutter) let `wl-copy` exit successfully without taking the clipboard; this is detected and the next tool is tried instead of reporting a false success.

`2o paste -c` on a server **without** a graphical session falls back to the tmux paste buffer (`prefix + ]`) when run inside tmux; otherwise it fails and suggests `2o paste > FILE` / `| CMD`. Install `wl-clipboard` (Wayland) or `xclip` (X11/XWayland) if neither is present.

## Limits, settings and trust

Size policy is decided by **the desktop** (the machine where things open); the server only announces sizes, and nothing is streamed before the desktop agrees. Set these in the environment of the desktop daemon (e.g. your shell profile, then restart it with `pkill -f '2o daemon'`):

| Variable | Default | Effect |
|---|---|---|
| `TWO_O_CONFIRM_BYTES` | `1G` | Above this, a notification with **Accept / Refuse** buttons asks before receiving. Ignoring it for 2 minutes, or having no notification daemon, counts as refuse. `0` = never ask. |
| `TWO_O_MAX_BYTES` | `0` (none) | Hard cap: larger transfers are refused without asking. |
| `TWO_O_CLIPBOARD_BYTES` | `16M` | Clipboard copy/paste through the tunnel (held in memory). |
| `TWO_O_CACHE_DAYS` | `7` | Received copies in `~/.cache/2o` older than this are deleted (at daemon start, then every 6 h). `0` = keep everything. Edit recovery copies are never deleted. |

Sizes accept `k`, `M`, `G`, `T` suffixes. Regardless of settings, a transfer that would leave less than 1 GiB free on the desktop disk is refused. The daemon logs to `~/.cache/2o/daemon.log`.

- OSC 52 fallback for `2o copy` without a tunnel is capped at 100 KiB; the terminal must support OSC 52.
- Processes running as your user on the server can read the session file and request permitted desktop actions. Do not use an untrusted remote account.
- Executable files, `.desktop` files, scripts and symlinks are refused for opening. A received path is **never** treated as a local path. On the desktop, the daemon's TCP listener is bound to `127.0.0.1` and serves only authenticated sessions.
- `2o edit` requires a graphical blocking editor on the desktop; headless desktop integrations fail rather than silently opening on the server.
- SSH servers with remote TCP forwarding disabled (`AllowTcpForwarding no`) cannot use this transport. Remote HTTPS localhost and WebSockets are not currently bridged.

See [PLAN.md](PLAN.md) for architecture, security review and milestone acceptance.
