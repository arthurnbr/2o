// Package proxy bridges a desktop browser to an HTTP(S) service bound to the
// loopback interface of a remote host.
//
// Design (deliberately boring): the remote `2o open http://localhost:PORT`
// keeps one control connection to the desktop daemon. The desktop serves a
// private loopback listener with net/http/httputil.ReverseProxy; whenever its
// http.Transport needs a new upstream connection it asks over the control
// connection, and the remote opens a fresh authenticated tunnel connection
// ("proxy-stream") that it splices byte for byte to the service. Each
// upstream TCP connection is therefore its own tunnel connection: no
// multiplexer, no head-of-line blocking, and keep-alive, chunked bodies, SSE,
// long polling and WebSocket upgrades all work because both ends speak plain
// HTTP/1.1 over an ordinary stream. The remote never parses HTTP; it can only
// reach the one service it validated, and the desktop decides every header.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arthurnbr/2o/internal/wire"
)

const (
	// dialTimeout bounds connecting to the service on the remote host and
	// waiting for the remote to open a tunnel stream.
	dialTimeout = 10 * time.Second
	// maxStreams caps concurrent tunnel connections of one bridge; browsers
	// open about six per host, plus WebSockets.
	maxStreams   = 32
	cookiePrefix = "2o_bridge_"
	startPath    = "/__2o/"
)

// ErrBridgeClosed is returned by Remote when the desktop ends the bridge.
var ErrBridgeClosed = errors.New("the desktop closed the bridge")

// Target is a validated service on the remote host's loopback interface.
type Target struct {
	url        *url.URL // as given, for paths and messages
	host       string   // canonical host:port, also the upstream Host header
	dial       string   // address to dial on the remote host
	port       string
	tls        bool
	serverName string
	insecure   bool
}

// ParseTarget accepts http and https URLs whose host is a loopback name or
// address and that carry an explicit port. The remote host is never inferred
// from the SSH peer: that peer may be reached through a jump host. For https,
// TWO_O_INSECURE_LOCALHOST=1 accepts self-signed development certificates.
func ParseTarget(raw string) (*Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a valid URL: %q", raw)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Host == "" {
		return nil, fmt.Errorf("only http:// and https:// localhost URLs can be bridged: %q", raw)
	}
	if u.User != nil {
		return nil, errors.New("localhost URLs with a user name or password are not supported")
	}
	name := normalizeHost(u.Hostname())
	if !IsLoopback(name) {
		return nil, fmt.Errorf("%s is not a loopback address of the server", u.Hostname())
	}
	if u.Port() == "" {
		return nil, errors.New("localhost URL needs an explicit port: http://localhost:PORT/path")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %q in %s", u.Port(), raw)
	}
	t := &Target{url: u, port: strconv.Itoa(port), tls: u.Scheme == "https", serverName: name,
		insecure: os.Getenv("TWO_O_INSECURE_LOCALHOST") == "1"}
	if ip := hostIP(name); ip != nil {
		t.host = net.JoinHostPort(ip.String(), t.port)
		t.dial = t.host
		t.serverName = ip.String()
	} else {
		t.host = net.JoinHostPort(name, t.port)
		// *.localhost is loopback by definition, but not every resolver knows.
		t.dial = net.JoinHostPort("localhost", t.port)
	}
	return t, nil
}

// String returns the service origin, e.g. http://localhost:3000.
func (t *Target) String() string { return t.url.Scheme + "://" + t.host }

// IsLoopback reports whether a URL host names this machine the way a browser
// would resolve it: localhost, *.localhost, 127.0.0.0/8 in any IPv4 notation a
// browser accepts (127.1, 0x7f.1, 2130706433…), ::1, ::ffff:127.x, and the
// unspecified addresses 0.0.0.0 and ::, which also reach the local host.
func IsLoopback(host string) bool {
	h := normalizeHost(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := hostIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}

// hostIP parses an IP literal, including the WHATWG URL IPv4 forms.
func hostIP(h string) net.IP {
	if ip := net.ParseIP(h); ip != nil {
		return ip
	}
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := ipv4Number(p)
		if !ok {
			return nil
		}
		nums[i] = n
	}
	last := len(nums) - 1
	var v uint64
	for i, n := range nums[:last] {
		if n > 255 {
			return nil
		}
		v |= n << (8 * (3 - i))
	}
	if nums[last] >= 1<<(8*(4-last)) {
		return nil
	}
	v |= nums[last]
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func ipv4Number(s string) (uint64, bool) {
	base := 10
	switch {
	case s == "":
		return 0, false
	case len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X"):
		s, base = s[2:], 16
		if s == "" {
			return 0, true
		}
	case len(s) >= 2 && s[0] == '0':
		s, base = s[1:], 8
	}
	n, err := strconv.ParseUint(s, base, 32)
	return n, err == nil
}

// Dial connects to the service from the remote host. For https it completes
// the TLS handshake, so the tunnel carries plain HTTP/1.1.
func (t *Target) Dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", t.dial)
	if err != nil || !t.tls {
		return c, err
	}
	tc := tls.Client(c, &tls.Config{ServerName: t.serverName, InsecureSkipVerify: t.insecure, NextProtos: []string{"http/1.1"}})
	if err = tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

// Describe turns a Dial error into a message for the person at the browser
// or the remote terminal.
func (t *Target) Describe(err error) string {
	var cert *tls.CertificateVerificationError
	switch {
	case errors.As(err, &cert):
		return fmt.Sprintf("%s presents a certificate this server does not trust (%v); for a self-signed development certificate run 2o open again with TWO_O_INSECURE_LOCALHOST=1", t, cert.Err)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("nothing is listening on %s on the server", t)
	default:
		return fmt.Sprintf("cannot reach %s on the server: %v", t, err)
	}
}

// Remote serves the remote end of a bridge until the desktop closes ctrl.
// For each stream the desktop asks for, it dials the service and calls
// connect(id, failure) to open the tunnel connection; failure carries the
// reason when the service could not be reached. warn reports such failures
// once per distinct message.
func Remote(ctrl net.Conn, t *Target, connect func(id, failure string) (net.Conn, error), warn func(error)) error {
	defer ctrl.Close()
	r := &remote{target: t, connect: connect, warn: warn, conns: make(map[net.Conn]struct{})}
	defer r.closeAll()
	for {
		m, err := wire.Read(ctrl)
		if err != nil {
			return ErrBridgeClosed
		}
		if m.Op == "stream" && m.Name != "" {
			go r.stream(m.Name)
		}
	}
}

type remote struct {
	target  *Target
	connect func(id, failure string) (net.Conn, error)
	warn    func(error)
	mu      sync.Mutex
	last    string
	closed  bool
	conns   map[net.Conn]struct{}
}

func (r *remote) report(msg string) {
	r.mu.Lock()
	repeat := msg == r.last
	r.last = msg
	r.mu.Unlock()
	if !repeat && msg != "" && r.warn != nil {
		r.warn(errors.New(msg))
	}
}

func (r *remote) track(cs ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	for _, c := range cs {
		r.conns[c] = struct{}{}
	}
	return true
}

func (r *remote) untrack(cs ...net.Conn) {
	r.mu.Lock()
	for _, c := range cs {
		delete(r.conns, c)
		c.Close()
	}
	r.mu.Unlock()
}

func (r *remote) closeAll() {
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
}

func (r *remote) stream(id string) {
	up, err := r.target.Dial(context.Background())
	failure := ""
	if err != nil {
		failure = r.target.Describe(err)
	}
	tunnel, err := r.connect(id, failure)
	if err != nil {
		if up != nil {
			up.Close()
		}
		r.report(fmt.Sprintf("could not open a tunnel stream to the desktop: %v", err))
		return
	}
	if up == nil {
		tunnel.Close()
		r.report(failure)
		return
	}
	r.report("")
	if !r.track(up, tunnel) {
		r.untrack(up, tunnel)
		return
	}
	splice(tunnel, up)
	r.untrack(up, tunnel)
}

// splice copies both ways until either side ends, then closes both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// Bridge is the desktop end: a private loopback HTTP listener.
type Bridge struct {
	target    *Target
	ctrl      net.Conn
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
	addr      string // 127.0.0.1:PORT
	port      string
	token     string
	cookie    string
	writeMu   sync.Mutex
	mu        sync.Mutex
	streams   map[*tunnelConn]struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// Listen validates remoteURL and binds the bridge on a free 127.0.0.1 port.
// Nothing is served until Serve; the caller opens StartURL in a browser
// meanwhile (the kernel queues early connections).
func Listen(ctrl net.Conn, remoteURL string) (*Bridge, error) {
	t, err := ParseTarget(remoteURL)
	if err != nil {
		return nil, err
	}
	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Bridge{target: t, ctrl: ctrl, listener: l, addr: l.Addr().String(), token: token,
		streams: make(map[*tunnelConn]struct{}), done: make(chan struct{})}
	_, b.port, _ = net.SplitHostPort(b.addr)
	b.cookie = cookiePrefix + b.port
	b.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return b.dialStream(ctx) },
		MaxConnsPerHost:       maxStreams,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite:        b.rewrite,
		Transport:      b.transport,
		FlushInterval:  -1, // SSE and long polls reach the browser at once
		ModifyResponse: b.modifyResponse,
		ErrorHandler:   b.proxyError,
	}
	b.server = &http.Server{
		Handler:           b.guard(proxy),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return b, nil
}

// URL is the bridged address of the requested page, without the secret.
func (b *Bridge) URL() string { return "http://" + b.addr + b.next() }

// StartURL opens the bridge in a browser: it carries the one-time secret,
// sets the bridge cookie and redirects to URL.
func (b *Bridge) StartURL() string {
	return "http://" + b.addr + startPath + b.token + "?" + url.Values{"next": {b.next()}}.Encode()
}

func (b *Bridge) next() string {
	p := b.target.url.EscapedPath()
	if p == "" {
		p = "/"
	}
	if b.target.url.RawQuery != "" {
		p += "?" + b.target.url.RawQuery
	}
	return p
}

// Serve answers browsers until the control connection closes (the remote
// command ended) or Close is called, then shuts everything down.
func (b *Bridge) Serve() error {
	served := make(chan error, 1)
	go func() { served <- b.server.Serve(b.listener) }()
	go func() {
		_, _ = io.Copy(io.Discard, b.ctrl)
		b.Close()
	}()
	err := <-served
	b.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the listener, every accepted browser connection and every
// tunnel stream.
func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		b.server.Close()
		b.listener.Close()
		b.ctrl.Close()
		b.mu.Lock()
		streams := make([]*tunnelConn, 0, len(b.streams))
		for s := range b.streams {
			streams = append(streams, s)
		}
		b.mu.Unlock()
		for _, s := range streams {
			s.Close() // also releases the Attach call holding it
		}
		b.transport.CloseIdleConnections()
	})
	return nil
}

func (b *Bridge) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS rebinding: a page on evil.example resolving to 127.0.0.1
		// would send its own Host header.
		if !strings.EqualFold(r.Host, b.addr) && !strings.EqualFold(r.Host, "localhost:"+b.port) {
			page(w, http.StatusMisdirectedRequest, "Wrong address", "This 2o bridge only answers at http://"+b.addr+"/.")
			return
		}
		if strings.HasPrefix(r.URL.Path, startPath) {
			if !equal(strings.TrimPrefix(r.URL.Path, startPath), b.token) {
				page(w, http.StatusForbidden, "Link expired", "This 2o bridge link is not valid. Run 2o open again on the server.")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: b.cookie, Value: b.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.Header().Set("Location", safeNext(r.URL.Query().Get("next")))
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(b.cookie); err != nil || !equal(c.Value, b.token) {
			page(w, http.StatusForbidden, "Private bridge", "This 2o bridge only serves the browser it was opened in. Run 2o open again on the server to get a new link.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeNext(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}

func (b *Bridge) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(&url.URL{Scheme: "http", Host: b.target.host})
	pr.Out.Host = b.target.host
	// Bridge cookies (of this and other bridges) never leave the desktop.
	var kept []string
	for _, line := range pr.Out.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			if part = strings.TrimSpace(part); part != "" && !strings.HasPrefix(part, cookiePrefix) {
				kept = append(kept, part)
			}
		}
	}
	pr.Out.Header.Del("Cookie")
	if len(kept) > 0 {
		pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
	}
	// Dev servers often check Origin on WebSocket upgrades and POSTs.
	origin := b.target.url.Scheme + "://" + b.target.host
	for _, self := range []string{"http://" + b.addr, "http://localhost:" + b.port} {
		if strings.EqualFold(pr.Out.Header.Get("Origin"), self) {
			pr.Out.Header.Set("Origin", origin)
		}
		if ref := pr.Out.Header.Get("Referer"); strings.HasPrefix(strings.ToLower(ref), self+"/") {
			pr.Out.Header.Set("Referer", origin+ref[len(self):])
		}
	}
}

// redirectError blocks a redirect to another service on the remote loopback.
type redirectError struct{ location string }

func (e *redirectError) Error() string { return "redirect to " + e.location }

func (b *Bridge) modifyResponse(res *http.Response) error {
	loc := res.Header.Get("Location")
	if loc == "" {
		return nil
	}
	rewritten, err := b.rewriteLocation(loc)
	if err != nil {
		return err
	}
	res.Header.Set("Location", rewritten)
	return nil
}

// rewriteLocation keeps the browser on the bridge: absolute redirects to the
// same remote service move to the bridge address, redirects to another
// remote loopback port would land on the desktop's own services and are
// refused. Other hosts and relative paths pass unchanged.
func (b *Bridge) rewriteLocation(loc string) (string, error) {
	u, err := browserURL(loc)
	if err != nil {
		// A browser may still read an authority from it: refuse rather
		// than let it reach the desktop's own loopback.
		if strings.Contains(loc, "//") || strings.Contains(loc, `\\`) {
			return "", &redirectError{location: loc}
		}
		return loc, nil
	}
	if u.Host == "" || !IsLoopback(u.Hostname()) {
		return loc, nil
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" || (u.Scheme == "" && b.target.tls) {
			port = "443"
		}
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = b.target.url.Scheme
	}
	if port != b.target.port || scheme != b.target.url.Scheme {
		return "", &redirectError{location: loc}
	}
	u.Scheme, u.Host, u.User = "http", b.addr, nil
	return u.String(), nil
}

// browserURL parses a Location value the way a browser on an http:// page
// would: control characters stripped, backslashes as slashes, any number of
// slashes before the authority.
func browserURL(loc string) (*url.URL, error) {
	loc = strings.TrimFunc(loc, func(r rune) bool { return r <= ' ' })
	loc = strings.NewReplacer("\t", "", "\n", "", "\r", "", "\\", "/").Replace(loc)
	if strings.HasPrefix(loc, "//") {
		loc = "//" + strings.TrimLeft(loc, "/")
	} else if i := strings.Index(loc, ":"); i > 0 {
		scheme := strings.ToLower(loc[:i])
		rest := loc[i+1:]
		trimmed := strings.TrimLeft(rest, "/")
		// "http:path" is relative on an http page; anything else with a
		// special scheme names an authority.
		if scheme == "https" || (scheme == "http" && trimmed != rest) {
			loc = scheme + "://" + trimmed
		}
	}
	return url.Parse(loc)
}

func (b *Bridge) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var redirect *redirectError
	switch {
	case errors.As(err, &redirect):
		page(w, http.StatusBadGateway, "Redirect blocked",
			fmt.Sprintf("%s redirected to %s, another service on the server that this bridge does not forward. Open it with its own 2o open command on the server.", b.target, redirect.location))
	case r.Context().Err() != nil:
		// The browser went away; nobody reads an answer.
	default:
		page(w, http.StatusBadGateway, "Service unavailable", err.Error())
	}
}

func page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>2o: %s</title><h1>%s</h1><p>%s</p>\n",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// Desktop side of tunnel streams: dialStream asks the remote over the control
// connection; the remote's new connection arrives through Attach.

type attached struct {
	conn    *tunnelConn
	failure string
}

var pending = struct {
	sync.Mutex
	m map[string]chan attached
}{m: make(map[string]chan attached)}

func take(id string) (chan attached, bool) {
	pending.Lock()
	defer pending.Unlock()
	ch, ok := pending.m[id]
	delete(pending.m, id)
	return ch, ok
}

type tunnelConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
	bridge *Bridge
}

func (t *tunnelConn) Close() error {
	t.once.Do(func() {
		t.Conn.Close()
		close(t.closed)
		if t.bridge != nil {
			t.bridge.mu.Lock()
			delete(t.bridge.streams, t)
			t.bridge.mu.Unlock()
		}
	})
	return nil
}

func (b *Bridge) dialStream(ctx context.Context) (net.Conn, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	ch := make(chan attached, 1)
	pending.Lock()
	pending.m[id] = ch
	pending.Unlock()
	b.writeMu.Lock()
	err = wire.Write(b.ctrl, wire.Message{Op: "stream", Name: id})
	b.writeMu.Unlock()
	if err != nil {
		take(id)
		return nil, ErrBridgeClosed
	}
	timer := time.NewTimer(dialTimeout)
	defer timer.Stop()
	select {
	case a := <-ch:
		if a.failure != "" {
			a.conn.Close()
			return nil, errors.New(a.failure)
		}
		b.mu.Lock()
		select {
		case <-b.done:
			b.mu.Unlock()
			a.conn.Close()
			return nil, ErrBridgeClosed
		default:
		}
		a.conn.bridge = b
		b.streams[a.conn] = struct{}{}
		b.mu.Unlock()
		return a.conn, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("the server did not open a tunnel stream in time")
	case <-b.done:
		err = ErrBridgeClosed
	}
	if _, ok := take(id); !ok {
		// Attach already claimed the id: its connection is (or will be)
		// in the buffered channel and nobody else will close it.
		a := <-ch
		a.conn.Close()
	}
	return nil, err
}

// Attach hands a tunnel connection opened by the remote for stream id to the
// bridge waiting for it, and blocks until that stream ends. failure, when not
// empty, is the remote's reason for not reaching the service.
func Attach(c net.Conn, id, failure string) error {
	ch, ok := take(id)
	if !ok {
		return errors.New("unknown or expired bridge stream")
	}
	t := &tunnelConn{Conn: c, closed: make(chan struct{})}
	ch <- attached{conn: t, failure: failure}
	<-t.closed
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
