# 2o — implementation plan (reviewed)

## Goal
`2o open report.pdf` over SSH opens a *local copy* with the local desktop's associated application. No inbound ports on either host, no SFTP re-authentication, and no dependency on the server's public IP (works over Tailscale/ProxyJump). Six capabilities: open, BROWSER integration, clipboard, edit, localhost, notifications.

## Architecture and decisions

- Go standard library for protocol and streaming; one binary on both Linux endpoints. Avoid yamux: a fresh connection per request over the SSH forward already supports concurrent operations. Avoid HTTP file serving; the forwarded TCP stream is carried inside SSH and authenticated per connection.
- Local `2o daemon` listens on a mode-0600 control Unix socket in `$XDG_RUNTIME_DIR` and an authenticated `127.0.0.1` TCP listener. `2o ssh HOST` registers a per-session secret, adds a remote **TCP** forward from a free server loopback port, writes the secret to a private session file over SSH stdin, checks the remote binary checksum and provisions the matching Linux amd64/arm64 binary when needed. Unix-socket remote forwards were dropped: Tailscale SSH creates them root-owned (mode 0600), which the user cannot connect to. Every TCP connection performs a mutual HMAC challenge (daemon proves first) so other local users of the server can neither use nor impersonate the tunnel.
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
- Go 1.26.6 is installed on `omarchy`; the originating machine may use a copied static binary instead. The server `78.123.62.14` is reachable from omarchy as `2serv-1` on **port 22**. The distinct `2serv-2` alias uses port 222, which returned `No route to host`; treating that failure as server-wide unavailability was incorrect.

## Milestones, acceptance

1. SSH transport, daemon, CLI: unique forwarded socket, SSH config compatibility; local Unix-socket integration test.
2. Open URL/file/directory: local launch, safe streamed copy, no execution; CLI smoke.
3. `$BROWSER` / `xdg-open`: real CLI dispatch via symlink or `2o open`.
4. Clipboard copy/paste and OSC 52 copy fallback; byte-for-byte tests.
5. Edit with wait, changed content round-trip, conflict rejection.
6. Localhost proxy preserves HTTP request and response over SSH, then notification delivery. (Notification is capability 6 in the original list; localhost is capability 5.)

Verification includes a real SSH remote Unix-socket forward from omarchy to `78.123.62.14:22`: `2o status`, URL/PDF/directory open, clipboard copy/paste, edit writeback, localhost HTTP proxy, and notification all completed. PDF and directory copies matched remote SHA-256 hashes; HTTP response came from the server's loopback listener. Local desktop actions were recorded with test commands, not a real graphical application, so this does not prove a real GUI launch on the user's SSH-origin machine.
