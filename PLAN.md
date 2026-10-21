# 2o — implementation plan (reviewed)

## Goal
`2o open report.pdf` over SSH opens a *local copy* with the local desktop's associated application. No inbound ports on either host, no SFTP re-authentication, and no dependency on the server's public IP (works over Tailscale/ProxyJump). Six capabilities: open, BROWSER integration, clipboard, edit, localhost, notifications.

## Architecture and decisions

- Go standard library for protocol and streaming; one binary on both Linux endpoints. Avoid yamux: a fresh connection per request over the SSH-forwarded Unix socket already supports concurrent operations. Avoid HTTP file serving and unencrypted TCP. SSH encrypts the forwarded stream.
- Local `2o daemon` listens on a mode-0600 Unix socket in `$XDG_RUNTIME_DIR`. `2o ssh HOST` adds `RemoteForward` to a *per-session* Unix socket in a mode-0700 remote directory, checks the remote binary checksum and provisions the matching Linux amd64/arm64 binary over SSH when needed (cross-build from this source checkout if the client architecture differs). It exports `TWO_O_SOCK` and `BROWSER` to the remote command/shell. The remote client never depends on the remote IP; `ssh` honors configured Tailscale hostnames, ProxyJump, custom ports, and identities.
- Framed protocol: bounded JSON header, exact byte length for files and clipboard, bounded response; no magic filename delimiters. One operation per connection. Reject oversized inputs and unknown operations; stream without buffering entire files.
- `open`: HTTP(S) URLs pass through; regular files stream into a private local cache directory, sanitized names, non-executable mode; directories stream as tar and extract under a private directory without following links. Never execute `.desktop`, executables or scripts by default; refuse unsafe types. `xdg-open`/`open` on desktop.
- `edit`: blocking graphical editor specified by `TWO_O_EDITOR`/`VISUAL`/`EDITOR` or detected on the desktop. The client receives replacement bytes only on success. Compare the remote original hash after receiving bytes and atomically replace, preserving mode. On conflict or lost acknowledgement, retain the edited local recovery copy.
- `localhost`: reverse HTTP proxy over the same SSH socket; the remote command maintains a connection while a loopback-only local listener is in use. Local browser never hits its own localhost service. Must relay HTTP paths, query, method, headers and response, not just rewrite the host.
- Clipboard: `wl-copy`/`wl-paste`, `xclip`, `pbcopy`/`pbpaste`; OSC 52 fallback *only for copy into the local terminal* when service is absent. Notifications use `notify-send` or `osascript`.

## Security review / adjustments to initial sketch

- `StreamLocalBindUnlink` is a client SSH option; do not change system sshd configuration. A unique socket per session avoids collisions instead.
- A remote server is not trusted merely because SSH authenticated it. Remote user processes can connect to the forwarded socket: restrict local actions and limit transferred size. Desktop opening is opt-in per command; never interpret received paths as local paths.
- Do not promise portal confirmation or graphical environment inheritance. A daemon started from a desktop session can use `xdg-open`; a headless daemon must fail visibly. Do not assume MIME alone prevents execution: block dangerous extensions and executable files.
- A local editor is not `xdg-open`: `edit` needs a real blocking editor, an explicit conflict check and failure propagation.
- A local reverse-forward *cannot* on its own reach remote localhost after the initiating command exits. Keep a live request stream for HTTP proxying; avoid silently opening the wrong service.
- Go 1.26.6 was found on the execution host `omarchy`, not verified on the user's SSH-origin desktop. SSH to `78.123.62.14` timed out or had no route **from omarchy**; the user reports it works from their own machine. Verify connection-specific behavior from the desktop that initiates SSH; loopback OpenSSH tests only prove the transport independently of that network route.

## Milestones, acceptance

1. SSH transport, daemon, CLI: unique forwarded socket, SSH config compatibility; local Unix-socket integration test.
2. Open URL/file/directory: local launch, safe streamed copy, no execution; CLI smoke.
3. `$BROWSER` / `xdg-open`: real CLI dispatch via symlink or `2o open`.
4. Clipboard copy/paste and OSC 52 copy fallback; byte-for-byte tests.
5. Edit with wait, changed content round-trip, conflict rejection.
6. Localhost proxy preserves HTTP request and response over SSH, then notification delivery. (Notification is capability 6 in the original list; localhost is capability 5.)

After every milestone, proceed to the next. Validate `78.123.62.14` from the user's SSH-origin desktop when accessible; do not extrapolate reachability from omarchy.
