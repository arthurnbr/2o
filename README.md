# 2o (to open)

Work on a server over SSH, and open things on **your own desktop**: files and folders in their desktop application, URLs in your browser, the server's `localhost:3000` in your browser, text in your clipboard, a file in your desktop editor with the result saved back on the server, a notification when a build ends.

Everything travels inside the SSH connection you already use: no open ports, no SFTP, no public IP needed (works with Tailscale, ProxyJump, SSH aliases and custom ports through your normal `ssh` configuration). Desktop: Linux (macOS best effort). Server: Linux amd64 or arm64; nothing to install by hand, no sudo.

## Quickstart

On your **desktop** (where things should open), with 2o installed (see [Install](#install)):

```sh
2o ssh HOST                 # instead of: ssh HOST — any ssh options work: 2o ssh -p 2222 user@HOST
```

The first connection installs 2o on HOST (`~/.local/bin/2o`). Then, in that remote shell:

```sh
2o status                   # which desktop this shell reaches
2o report.pdf               # open on the desktop (same as: 2o open report.pdf)
2o edit app.go:42           # edit in the desktop editor, saved back on the server
git diff | 2o copy          # to the desktop clipboard
```

`2o ssh` must run on the machine whose desktop you want: that can be a host you reached through SSH if its graphical session is available, but files open **where `2o ssh` ran**, not at the start of an SSH chain. An SSH connection that is already open cannot be retrofitted.

## Commands

```text
2o ssh [ssh options] HOST [command…]   connect to HOST with a tunnel back to this desktop
2o open FILE|DIR|URL                    open on the desktop (2o FILE works too)
2o edit [-n] FILE[:LINE]                edit in the desktop's editor, saved back here
2o copy < DATA                          put stdin on the desktop clipboard
2o paste [-c]                           print the desktop clipboard
2o notify TITLE BODY                    show a desktop notification
2o status [-v]                          show which desktop this shell reaches
2o doctor                               check the tunnel and the desktop's tools
```

`2o --help` (or `2o`, `2o help`) lists them; `2o COMMAND --help` explains one; `2o --version` prints the version. Help goes to stdout and never runs anything, so `printf x | 2o copy --help` copies nothing. A single argument that is neither a command nor an existing path or URL is an error (`unknown command "opne"` → `did you mean: 2o open`), not a file to open.

### open

```sh
2o open https://example.com          # desktop browser
2o open report.pdf                   # copied to the desktop cache, opened there
2o open ./folder                     # streamed as tar, extracted safely, opened
2o open http://localhost:3000/page   # the server's port 3000 in the desktop browser
```

Files and folders are **copies** in the desktop's `~/.cache/2o`: changes are not sent back (use `2o edit`). Executables, scripts, `.desktop` files and symlinks are refused. A received path is never treated as a desktop path.

`2o open http://localhost:PORT` (also `127.0.0.1`, `[::1]`, `https://localhost:PORT`) starts a bridge: a private loopback listener on the desktop forwards each browser request to the server's port, and the command **stays running** until Ctrl-C closes the bridge. The port must be explicit.

In `2o ssh` shells, `BROWSER` and an `xdg-open` that runs `2o` are provided from the session directory (`/tmp/2o-UID-ID/`, first on `PATH` for that session only), so tools that open a browser (`gh auth login`, `jupyter`, …) open it on the desktop. Nothing is left in `~/.local/bin` besides `2o` itself.

### edit

```sh
2o edit notes.txt          # open in the desktop editor; save and close it to apply
2o edit app.go:42          # at line 42 (code, codium, zed, subl, kate, gvim, gedit, emacs)
2o edit -n new.txt         # create the file if it does not exist
```

The desktop opens a private copy in a **graphical editor that blocks until the file is closed**. The editor is chosen on the desktop: `TWO_O_EDITOR` (e.g. `code --wait`, `gvim -f`, `kate --block`), else a detected one (code, codium, zed, gvim, kate, gedit), else `VISUAL`/`EDITOR` when they name a known graphical editor. Terminal editors (vim, nano, …) are refused: the daemon has no terminal.

When you close the editor, the server file is replaced atomically (mode kept) **only if it did not change meanwhile**. Your work is never dropped: the desktop keeps the edited copy until the server confirms the write. If the editor fails, the connection drops, or the file changed on the server, 2o says where your edits are:

```text
✗ notes.txt changed on the server while you were editing; it was not overwritten
  → your edits are safe in /home/me/.cache/2o/edit-123/notes.txt on me@laptop
  → merge your edits, then run 2o edit again
```

Recovery copies (`~/.cache/2o/edit-*`) are never cleaned automatically.

### copy, paste

```sh
printf 'hello' | 2o copy             # server stdin → desktop clipboard (silent on success)
2o paste > clip.txt                  # desktop clipboard → stdout (file, pipe, terminal)
2o paste -c                          # desktop clipboard → this server's clipboard or tmux buffer
```

Without a tunnel, `2o copy` falls back to the terminal clipboard (OSC 52, at most 100 KiB, if your terminal allows it) — only when stdout is a terminal, so escape codes never end up in a pipe or file.

The clipboard code finds the graphical session even from an SSH shell or a daemon started by systemd (environment → `systemctl --user show-environment` → the user's Wayland/X11 sockets owned by you), then uses `wl-copy`/`wl-paste` (Wayland), `xclip` or `xsel` (X11 and XWayland), or `pbcopy` (macOS). Each copy is read back: some compositors (GNOME/Mutter) let `wl-copy` succeed without taking the clipboard, and the next tool is tried. On Plasma, Klipper is used to **read** only (copy goes through `wl-copy` or XWayland `xclip`: Klipper's D-Bus copy takes the text as a command-line argument, visible to other processes). Install `wl-clipboard` or `xclip` if neither is present. `2o paste -c` on a headless server uses the tmux buffer (`prefix + ]`) when inside tmux; otherwise use `2o paste > FILE` or `2o paste | CMD`.

### notify, status, doctor

```sh
make; 2o notify build "done: $?"
2o status                  # ✓ connected to me@laptop, its version, session age; exit 3 without a tunnel
2o doctor                  # or 2o status -v
```

`2o doctor` checks this end (2o version, session file, tunnel) and asks the desktop to check itself: graphical session, opener, clipboard tools, notifications (and whether they support Accept/Refuse buttons), editor, transfer policy, daemon log. It changes nothing. On the desktop itself it runs the desktop checks directly.

## What you see

Progress and errors go to **stderr** (colours and in-place updates on a terminal, plain lines otherwise); stdout carries only data (`paste`, help, `--version`). Data commands (`copy`, `paste`, `notify`) are silent on success.

```text
$ 2o ssh myserver                                     # on the desktop
2o → myserver
– desktop daemon already running
✓ connecting to myserver
– 2o is up to date on myserver                        # or: installing 2o on myserver (10.6 MiB)
✓ opening tunnel
✓ desktop tunnel ready — files, links and clipboard now open on me@laptop

$ 2o report.pdf
✓ opened report.pdf on me@laptop                       # after "⠹ sending report.pdf (2.3 MiB) to me@laptop"

$ 2o big.iso
⠼ sending big.iso to me@laptop — 42% (1.3 GiB of 3.1 GiB)
! waiting for confirmation on me@laptop…               # above TWO_O_CONFIRM_BYTES

$ 2o edit notes.txt
• editing notes.txt on me@laptop — save and close the editor to apply
✓ notes.txt updated

$ 2o open http://localhost:3000
✓ http://localhost:3000 → opened on me@laptop (bridge 127.0.0.1:41234) — Ctrl-C to close

$ 2o status
✓ connected to me@laptop
  desktop  me@laptop
  version  v0.4.0
  session  12 min (/tmp/2o-1000-…/session)
```

Errors are one line, then what to do:

```text
✗ no desktop tunnel in this shell
  → connect from your desktop with: 2o ssh HOST
```

Set `TWO_O_QUIET=1` (or `2o ssh -q`) to keep only warnings and errors; `NO_COLOR=1` disables colours.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | the action failed (message on stderr) |
| 2 | usage error: unknown command or option, wrong arguments, missing file, ssh modes 2o cannot wrap (`-N -f -W -O -Q -G -V -s -M -S`) |
| 3 | no tunnel: no 2o session in this shell, or the desktop is unreachable |
| 4 | refused by the desktop: size limit, free space, declined or unanswered confirmation |
| N | `2o ssh HOST command`: the remote command's own exit code (never re-run) |
| 255 | `2o ssh`: ssh itself failed (as with ssh) |

## Settings

Transfer limits are decided by **the desktop**; the server only announces sizes, and nothing is sent before the desktop agrees. Set them in the desktop daemon's environment (e.g. your shell profile); the daemon reads them when it starts, and it keeps its port across restarts, so after `pkill -f '2o daemon'` the next `2o ssh` starts a new one and open sessions reconnect.

| Variable | Where | Default | Effect |
|---|---|---|---|
| `TWO_O_EDITOR` | desktop | detected | Blocking graphical editor for `2o edit`, e.g. `code --wait`. |
| `TWO_O_CONFIRM_BYTES` | desktop | `1G` | Above this, a notification with **Accept / Refuse** asks before receiving. No answer in 2 minutes = refused ("nobody answered"); no notification daemon = refused. `0` = never ask. |
| `TWO_O_MAX_BYTES` | desktop | `0` (none) | Hard cap on what is being received at once; larger transfers are refused without asking. |
| `TWO_O_CLIPBOARD_BYTES` | desktop | `16M` | Clipboard size for `copy`/`paste` (held in memory). |
| `TWO_O_CACHE_DAYS` | desktop | `7` | Received copies in `~/.cache/2o` older than this are deleted (at daemon start, then every 6 h). `0` = keep everything. Edit recovery copies are never deleted. |
| `TWO_O_QUIET` | both | unset | `1`: only warnings and errors on stderr. |
| `NO_COLOR` | both | unset | No colours. |
| `TWO_O_SESSION` | server | set by `2o ssh` | The session file this shell uses. In a reattached tmux whose session is gone, 2o only moves to another live session of the **same** desktop; otherwise it lists them so you pick one with `export TWO_O_SESSION=…`. |
| `TWO_O_DESKTOP` | server | set by `2o ssh` | Label (`user@host`) of the desktop of this shell, for messages. |
| `BROWSER` | server | set by `2o ssh` | Makes tools open URLs through 2o. |

Sizes accept `k`, `M`, `G`, `T` suffixes. Whatever the settings, a transfer that would leave less than 1 GiB free on the desktop disk is refused. The desktop daemon logs to `~/.cache/2o/daemon.log`.

## Install

2o is one static binary. Install it on the **desktop**; `2o ssh` installs the server copy itself. It uploads its own binary when the server has the same CPU; for a server with another CPU it uploads the matching prebuilt `2o-linux-amd64` or `2o-linux-arm64`, looked up next to the `2o` binary, in `../lib/2o/`, then in `~/.cache/2o/bin/`. Run from inside the source checkout with Go installed, `2o ssh` builds the missing one once into that cache.

From source (Go 1.25+):

```sh
git clone https://github.com/arthurnbr/2o.git && cd 2o
make install                 # ~/.local/bin/2o + ~/.local/lib/2o/2o-linux-{amd64,arm64}
# or: sudo make PREFIX=/usr/local install
```

`make` alone builds `build/2o` (this machine) and `build/2o-linux-amd64`, `build/2o-linux-arm64` (servers). Make sure `~/.local/bin` is on your `PATH`.

Arch Linux: [`packaging/aur/2o-git/PKGBUILD`](packaging/aur/2o-git/PKGBUILD) builds the same set (`/usr/bin/2o`, `/usr/lib/2o/2o-linux-{amd64,arm64}`):

```sh
cd packaging/aur/2o-git && makepkg -si
```

Without Go on the desktop: build on any machine with Go (`make`, or `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o 2o .` for one architecture — `uname -m`: `x86_64` → `amd64`, `aarch64` → `arm64`), then copy `build/2o` and the `build/2o-linux-*` files into one directory on the desktop's `PATH`.

## How it works, and what to trust

`2o ssh` uses one private SSH master connection (you authenticate once), starts a daemon in the desktop session if needed, registers a fresh per-session secret with it for as long as the SSH session lives (revoked when it ends, even if the wrapper is killed), and adds an SSH **remote TCP forward** from a loopback port on the server to the daemon's loopback listener. The secret reaches the server over SSH stdin into a private session file (`/tmp/2o-UID-ID/session`, mode 0600 in a 0700 directory). Before the remote shell starts, the tunnel is checked end to end.

The forward is TCP rather than a Unix socket because Tailscale SSH creates forwarded Unix sockets as root with mode 0600, unusable by the logged-in user. A loopback port is reachable by every local user of the server, so every connection runs a mutual HMAC challenge with fresh nonces from both sides, bound to the request: the daemon proves it knows the secret before anything is sent (an impostor on a stale port gets nothing), the client proves it before the daemon acts, and a captured exchange cannot be replayed or altered.

- Processes running as your user on the server can read the session file and ask for the actions above. Do not use 2o from an untrusted remote account.
- The desktop decides: size caps, free space, confirmation. The daemon's TCP listener is bound to `127.0.0.1`; its control socket is in `$XDG_RUNTIME_DIR`.
- The localhost bridge serves only the desktop browser that opened it (private token, strict `Host` check).
- Servers with `AllowTcpForwarding no` cannot use 2o.

See [PLAN.md](PLAN.md) for architecture and design decisions.
