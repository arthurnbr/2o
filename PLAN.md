# 2o — implementation plan (reviewed)

## Goal
`2o open report.pdf` over SSH opens a *local copy* with the local desktop's associated application. No inbound ports on either host, no SFTP re-authentication, and no dependency on the server's public IP (works over Tailscale/ProxyJump). Six capabilities: open, BROWSER integration, clipboard, edit, localhost, notifications.

## Architecture and decisions

- Go standard library for protocol and streaming; one binary on both Linux endpoints. Avoid yamux: a fresh connection per request over the SSH forward already supports concurrent operations. Avoid HTTP file serving; the forwarded TCP stream is carried inside SSH and authenticated per connection.
- Local `2o daemon` listens on a mode-0600 control Unix socket in `$XDG_RUNTIME_DIR` (local only, never forwarded) and an authenticated `127.0.0.1` TCP listener whose port is kept across daemon restarts. `2o ssh HOST` holds a per-session secret on the control socket for the life of the session (revoked when the wrapper exits, even on `kill -9`), adds a remote **TCP** forward from a free server loopback port, writes the secret to a private session file over SSH stdin, checks the remote binary checksum and provisions the matching Linux amd64/arm64 binary when needed. Unix-socket remote forwards were dropped: Tailscale SSH creates them root-owned (mode 0600), which the user cannot connect to. Every TCP connection performs a mutual HMAC challenge with a fresh nonce from each side, the client proof bound to the request header (daemon proves first), so other local users of the server can neither use, replay nor impersonate the tunnel.
- Framed protocol: bounded JSON header, exact byte length for files and clipboard, bounded response; no magic filename delimiters. One operation per connection. Reject oversized inputs and unknown operations; stream without buffering entire files.
- `open`: HTTP(S) URLs pass through; regular files stream into a private local cache directory, sanitized names, non-executable mode; directories stream as tar and extract under a private directory without following links. Never execute `.desktop`, executables or scripts by default; refuse unsafe types. `xdg-open`/`open` on desktop.
- `edit`: blocking graphical editor (`TWO_O_EDITOR`, else a detected one; terminal editors refused) on the desktop. The desktop copy is kept from the moment the editor starts and deleted only after the server confirms the write (or when the editor left it unchanged); editor failure, transfer failure, conflict or disconnect all report the recovery path. The server hashes exactly the bytes it sent (same open descriptor, `O_NOFOLLOW`, fstat identity check), compares before an atomic replace preserving mode. `-n` creates a missing file without ever replacing one; `FILE:LINE` opens at a line for known editors.
- `localhost`: reverse HTTP proxy over the same SSH TCP forward; the remote command maintains a control connection while a loopback-only local listener is in use, and each browser request gets its own tunnel stream (SSE/WebSocket do not block others). Local browser never hits its own localhost service. Must relay HTTP paths, query, method, headers and response, not just rewrite the host.
- Clipboard: `wl-copy`/`wl-paste`, `xclip`, `pbcopy`/`pbpaste`; OSC 52 fallback *only for copy into the local terminal* when service is absent. Notifications use `notify-send` or `osascript`.

## Security review / adjustments to initial sketch

- No sshd configuration change: the transport is a per-session loopback TCP remote forward (remote Unix-socket forwards were abandoned, see above).
- A remote server is not trusted merely because SSH authenticated it. Remote user processes can connect to the forwarded socket: restrict local actions and limit transferred size. Desktop opening is opt-in per command; never interpret received paths as local paths.
- Do not promise portal confirmation or graphical environment inheritance. A daemon started from a desktop session can use `xdg-open`; a headless daemon must fail visibly. Do not assume MIME alone prevents execution: block dangerous extensions and executable files.
- A local editor is not `xdg-open`: `edit` needs a real blocking editor, an explicit conflict check and failure propagation.
- A local reverse-forward *cannot* on its own reach remote localhost after the initiating command exits. Keep a live request stream for HTTP proxying; avoid silently opening the wrong service.
- Go 1.26.6 is installed on `omarchy`; the originating machine may use a copied static binary instead. The server `78.123.62.14` is reachable from omarchy as `2serv-1` on **port 22**. The distinct `2serv-2` alias uses port 222, which returned `No route to host`; treating that failure as server-wide unavailability was incorrect.

## Milestones, acceptance

1. SSH transport, daemon, CLI: per-session loopback TCP forward, SSH config compatibility; integration tests run the real daemon loop over loopback TCP.
2. Open URL/file/directory: local launch, safe streamed copy, no execution; CLI smoke.
3. `$BROWSER` / `xdg-open`: real CLI dispatch via symlink or `2o open`.
4. Clipboard copy/paste and OSC 52 copy fallback; byte-for-byte tests.
5. Edit with wait, changed content round-trip, conflict rejection.
6. Localhost proxy preserves HTTP request and response over SSH, then notification delivery. (Notification is capability 6 in the original list; localhost is capability 5.)

Initial verification (before the switch to TCP, commit 574228c) used a real SSH remote Unix-socket forward from omarchy to `78.123.62.14:22`; the same scenarios were then repeated over the loopback TCP forward: `2o status`, URL/PDF/directory open, clipboard copy/paste, edit writeback, localhost HTTP proxy, and notification all completed. PDF and directory copies matched remote SHA-256 hashes; HTTP response came from the server's loopback listener. Local desktop actions were recorded with test commands, not a real graphical application, so this does not prove a real GUI launch on the user's SSH-origin machine.

Transfer policy (desktop-side, before any payload): `TWO_O_MAX_BYTES` hard cap, free-disk check keeping 1 GiB, notification Accept/Refuse above `TWO_O_CONFIRM_BYTES` (default 1 GiB; no answer or no notifier = refuse), `TWO_O_CLIPBOARD_BYTES` (16 MiB, in memory), cache cleanup after `TWO_O_CACHE_DAYS` (7). Verified on `78.123.62.14`: refused 3 MiB transfer left nothing in cache, accepted one matched the server hash, cap refusal without prompt.

## Review fixes (REVIEW.md)

Status of the multi-aspect review items handled in the CLI slice; the other slices report theirs to the integrator, who updates REVIEW.md.

- **P0 #2 edit loses saved work** — fixed: the desktop copy is kept from the moment the editor starts, deleted only after the server confirms the commit; every failure path prints "your edits are safe in PATH on DESKTOP". Regression tests: `TestEditorFailureAfterSaveKeepsEdits`, `TestEditConflictKeepsRecoveryCopy`, `TestEditNewFileRaceKeepsEdits`.
- **P1 #11 status does not show the destination** — fixed on the CLI side: `2o status` prints the target desktop, its 2o version and the session age (routing itself: Session slice).
- **P1 #17 bridge open failure exits 0 / "EOF"** — errors now travel as structured replies (message, hints, exit code) and are printed by the remote CLI (bridge: Proxy slice).
- **P2 help** — fixed: `2o`, `2o help [CMD]`, `-h/--help`, `2o CMD --help` print usage on stdout (exit 0, no side effects); unknown commands exit 2 with a "did you mean"; every command validates its arguments (`copy --help` no longer reads stdin); internal commands (`daemon`, `shell`) are hidden; `free-port` is gone (sshd picks the forward port); `2o --version`.
- **P2 silent operations** — fixed: open/edit show progress on stderr (sending, percentage on a terminal, opened/updated), and "waiting for confirmation on DESKTOP…" while the desktop asks.
- **P2 status -v / doctor** — fixed: `2o doctor` (= `status -v`) checks session, tunnel, versions and, on the desktop, graphical session, opener, clipboard, notifications, editor, transfer policy and log.
- **P2 OSC 52 written into pipes** — fixed: the `copy` fallback only writes OSC 52 when stdout is a terminal; otherwise exit 3 with a hint.
- **P2 messages** — "unknown command" names the command; remote errors keep their hints and exit codes.
- **P2 distribution / quickstart** — README: generic HOST, `make install`, AUR PKGBUILD, prebuilt `2o-linux-{amd64,arm64}` names.
- **P2 docs** — this file no longer cites the Unix-socket forward as the current transport.
