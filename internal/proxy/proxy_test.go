package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// bridge wires Listen/Serve (desktop) to Remote (server) the way the daemon
// does: one control pipe, and one pipe per stream handed to Attach.
type bridge struct {
	*Bridge
	client     *http.Client // carries the bridge cookie
	remoteDone chan error
	served     chan error
	warnings   chan error
}

func startBridge(t *testing.T, remoteURL string) *bridge {
	t.Helper()
	desk, srv := net.Pipe()
	b, err := Listen(desk, remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ParseTarget(remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	tb := &bridge{Bridge: b, remoteDone: make(chan error, 1), served: make(chan error, 1), warnings: make(chan error, 16)}
	connect := func(id, failure string) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() { _ = Attach(local, id, failure) }()
		return remote, nil
	}
	warn := func(e error) {
		select {
		case tb.warnings <- e:
		default:
		}
	}
	go func() { tb.remoteDone <- Remote(srv, target, connect, warn) }()
	go func() { tb.served <- b.Serve() }()
	jar, _ := cookiejar.New(nil)
	tb.client = &http.Client{Jar: jar, Timeout: 5 * time.Second}
	t.Cleanup(func() {
		b.Close()
		select {
		case <-tb.remoteDone:
		case <-time.After(3 * time.Second):
			t.Error("remote end did not stop after the bridge closed")
		}
	})
	return tb
}

// login follows the start URL like a browser and returns the landing page.
func (tb *bridge) login(t *testing.T) (*http.Response, string) {
	t.Helper()
	res, err := tb.client.Get(tb.StartURL())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(body)
}

func (tb *bridge) get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	res, err := tb.client.Get("http://" + tb.addr + path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res.StatusCode, res.Header, string(body)
}

func TestStartURLSetsCookieAndForwardsWithoutIt(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s host=%s cookie=%q body=%s", r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("Cookie"), body)
	}))
	defer service.Close()
	tb := startBridge(t, service.URL+"/initial%20path?before=1")

	if got := tb.URL(); got != "http://"+tb.addr+"/initial%20path?before=1" {
		t.Fatalf("URL() = %q", got)
	}
	if strings.Contains(tb.URL(), tb.token) {
		t.Fatal("URL() leaks the bridge secret")
	}
	res, body := tb.login(t)
	host := strings.TrimPrefix(service.URL, "http://")
	if res.StatusCode != 200 || body != `GET /initial%20path?before=1 host=`+host+` cookie="" body=` {
		t.Fatalf("landing: %d %q", res.StatusCode, body)
	}
	var set *http.Cookie
	for _, c := range tb.client.Jar.Cookies(&url.URL{Scheme: "http", Host: tb.addr}) {
		if c.Name == tb.cookie {
			set = c
		}
	}
	if set == nil {
		t.Fatal("bridge cookie not stored")
	}

	// The app's own cookies pass; bridge cookies never reach the service.
	req, _ := http.NewRequest(http.MethodPost, "http://"+tb.addr+"/x", strings.NewReader("payload"))
	req.Header.Set("Cookie", "app=1; "+tb.cookie+"="+tb.token+"; 2o_bridge_1=other")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != `POST /x host=`+host+` cookie="app=1" body=payload` {
		t.Fatalf("forwarded request: %q", b)
	}
}

func TestMissingOrWrongCookieIsForbidden(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request without the bridge cookie reached the service")
	}))
	defer service.Close()
	tb := startBridge(t, service.URL)
	for _, cookie := range []string{"", tb.cookie + "=" + strings.Repeat("0", len(tb.token))} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+tb.addr+"/", nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "2o open") {
			t.Fatalf("cookie %q: %d %q", cookie, res.StatusCode, body)
		}
	}
	// A guessed start link does not set the cookie either.
	res, err := http.Get("http://" + tb.addr + startPath + strings.Repeat("f", len(tb.token)) + "?next=/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || len(res.Cookies()) != 0 {
		t.Fatalf("wrong token: %d cookies=%v", res.StatusCode, res.Cookies())
	}
}

func TestForeignHostIsRejected(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rebinding request reached the service")
	}))
	defer service.Close()
	tb := startBridge(t, service.URL)
	for _, host := range []string{"evil.example:" + tb.port, "127.0.0.1", "127.0.0.2:" + tb.port} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+tb.addr+"/", nil)
		req.Host = host
		req.Header.Set("Cookie", tb.cookie+"="+tb.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("Host %q: status %d", host, res.StatusCode)
		}
	}
	// localhost:PORT is the other name of the bridge.
	req, _ := http.NewRequest(http.MethodGet, "http://"+tb.addr+startPath+tb.token+"?next=/", nil)
	req.Host = "localhost:" + tb.port
	res, err := (&http.Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("localhost:PORT start: %d", res.StatusCode)
	}
}

func TestStartRedirectStaysOnBridge(t *testing.T) {
	for next, want := range map[string]string{"/a?b=1": "/a?b=1", "//evil.example/": "/", "/\\evil.example": "/", "https://evil.example": "/"} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestRedirectRewriting(t *testing.T) {
	var port string
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loc := r.URL.Query().Get("to"); loc != "" {
			w.Header().Set("Location", strings.ReplaceAll(loc, "PORT", port))
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Fprint(w, "landed "+r.URL.RequestURI())
	}))
	defer service.Close()
	_, port, _ = net.SplitHostPort(strings.TrimPrefix(service.URL, "http://"))
	tb := startBridge(t, service.URL)
	tb.login(t)
	tb.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	same := []string{
		"http://127.0.0.1:PORT/p?q=1", "//localhost:PORT/p?q=1", "http://127.1:PORT/p?q=1",
		"http://[::1]:PORT/p?q=1", "http://[::ffff:127.0.0.1]:PORT/p?q=1", "http://app.localhost:PORT/p?q=1",
		"http://0x7f.0.0.1:PORT/p?q=1", "http:\\\\127.0.0.1:PORT/p?q=1", "HTTP://LOCALHOST.:PORT/p?q=1",
	}
	for _, loc := range same {
		status, h, _ := tb.get(t, "/?to="+url.QueryEscape(loc))
		if status != http.StatusFound || h.Get("Location") != "http://"+tb.addr+"/p?q=1" {
			t.Errorf("%s: %d Location=%q", loc, status, h.Get("Location"))
		}
	}
	other := []string{"http://127.0.0.1:1/x", "http://127.1:1/x", "//[::1]:1/x", "http://localhost/x", "https://127.0.0.1:PORT/x", "http://2130706433:1/x"}
	for _, loc := range other {
		status, h, body := tb.get(t, "/?to="+url.QueryEscape(loc))
		if status != http.StatusBadGateway || h.Get("Location") != "" || !strings.Contains(body, "Redirect blocked") {
			t.Errorf("%s: %d Location=%q body=%q", loc, status, h.Get("Location"), body)
		}
	}
	for _, loc := range []string{"/relative?x", "https://example.com/elsewhere"} {
		status, h, _ := tb.get(t, "/?to="+url.QueryEscape(loc))
		if status != http.StatusFound || h.Get("Location") != loc {
			t.Errorf("%s: %d Location=%q", loc, status, h.Get("Location"))
		}
	}
}

// A never-ending event stream must not hold up other requests.
func TestStreamingDoesNotBlockOtherRequests(t *testing.T) {
	release := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			fmt.Fprint(w, "ok "+r.URL.Query().Get("id"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; ; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			select {
			case <-release:
				return
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer service.Close()
	defer close(release)
	tb := startBridge(t, service.URL)
	tb.login(t)

	res, err := tb.client.Get("http://" + tb.addr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	events := bufio.NewReader(res.Body)
	if line, err := events.ReadString('\n'); err != nil || line != "data: 0\n" {
		t.Fatalf("first event %q, %v (not flushed?)", line, err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			status, _, body := tb.get(t, "/short?id="+id)
			if status != 200 || body != "ok "+id {
				t.Errorf("short request %s: %d %q", id, status, body)
			}
		}()
	}
	wg.Wait()
	// The stream is still alive afterwards.
	for range 3 {
		if _, err := events.ReadString('\n'); err != nil {
			t.Fatalf("stream died: %v", err)
		}
	}
}

func TestWebSocketUpgradeEchoes(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Origin: %s\r\n\r\n", r.Header.Get("Origin"))
		rw.Flush()
		_, _ = io.Copy(conn, rw) // echo
	}))
	defer service.Close()
	tb := startBridge(t, service.URL)

	conn, err := net.Dial("tcp", tb.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nOrigin: http://%s\r\nCookie: %s=%s\r\n\r\n", tb.addr, tb.addr, tb.cookie, tb.token)
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Origin"); got != service.URL {
		t.Errorf("service saw Origin %q, want %q", got, service.URL)
	}
	for _, msg := range []string{"hello", "second frame"} {
		if _, err = io.WriteString(conn, msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err = io.ReadFull(r, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo %q: got %q, %v", msg, buf, err)
		}
	}
}

// Closing the bridge must cut browser connections that are already
// accepted, idle or upgraded, not only the listener.
func TestCloseKillsAcceptedConnections(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			conn, rw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer service.Close()
	tb := startBridge(t, service.URL)

	idle, err := net.Dial("tcp", tb.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	kept, err := net.Dial("tcp", tb.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	fmt.Fprintf(kept, "GET / HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n\r\n", tb.addr, tb.cookie, tb.token)
	keptReader := bufio.NewReader(kept)
	res, err := http.ReadResponse(keptReader, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(res.Body)
	res.Body.Close()
	upgraded, err := net.Dial("tcp", tb.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	fmt.Fprintf(upgraded, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: x\r\nConnection: Upgrade\r\nCookie: %s=%s\r\n\r\n", tb.addr, tb.cookie, tb.token)
	upReader := bufio.NewReader(upgraded)
	if res, err = http.ReadResponse(upReader, nil); err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	time.Sleep(50 * time.Millisecond) // let the server register the idle conn

	tb.Close()
	select {
	case err := <-tb.served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
	for name, r := range map[string]io.Reader{"idle": idle, "keep-alive": keptReader, "upgraded": upReader} {
		conn := map[string]net.Conn{"idle": idle, "keep-alive": kept, "upgraded": upgraded}[name]
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := r.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("%s connection still open after Close: %v", name, err)
		}
	}
}

// When the server-side process ends, the desktop listener must go away.
func TestRemoteExitClosesBridge(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer service.Close()
	desk, srv := net.Pipe()
	b, err := Listen(desk, service.URL)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- b.Serve() }()
	srv.Close()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge kept serving after the control connection closed")
	}
	if c, err := net.DialTimeout("tcp", b.addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("listener still accepts connections")
	}
}

func TestUnreachableServiceIsExplained(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	tb := startBridge(t, "http://"+addr)
	tb.login(t)
	status, _, body := tb.get(t, "/")
	if status != http.StatusBadGateway || !strings.Contains(body, "nothing is listening on http://"+addr) {
		t.Fatalf("%d %q", status, body)
	}
	select {
	case w := <-tb.warnings:
		if !strings.Contains(w.Error(), "nothing is listening") {
			t.Fatalf("warning %v", w)
		}
	case <-time.After(time.Second):
		t.Fatal("remote terminal was not told why the page failed")
	}
}

func TestHTTPSUpstreamCertificatePolicy(t *testing.T) {
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secure %s %s", r.Host, r.URL.RequestURI())
	}))
	defer service.Close()

	t.Setenv("TWO_O_INSECURE_LOCALHOST", "")
	tb := startBridge(t, service.URL+"/p")
	res, body := tb.login(t)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "TWO_O_INSECURE_LOCALHOST=1") {
		t.Fatalf("self-signed without opt-in: %d %q", res.StatusCode, body)
	}
	select {
	case w := <-tb.warnings:
		if !strings.Contains(w.Error(), "TWO_O_INSECURE_LOCALHOST=1") {
			t.Fatalf("warning %v", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no warning on the server terminal")
	}

	t.Setenv("TWO_O_INSECURE_LOCALHOST", "1")
	tb = startBridge(t, service.URL+"/p")
	res, body = tb.login(t)
	if res.StatusCode != 200 || body != "secure "+strings.TrimPrefix(service.URL, "https://")+" /p" {
		t.Fatalf("self-signed with opt-in: %d %q", res.StatusCode, body)
	}
	if !strings.HasPrefix(tb.URL(), "http://127.0.0.1:") {
		t.Fatalf("browser side must stay plain http: %s", tb.URL())
	}
}

func TestParseTarget(t *testing.T) {
	accept := map[string]string{
		"http://localhost:3000/x":         "http://localhost:3000",
		"http://LOCALHOST:3000":           "http://localhost:3000",
		"http://127.1:8080":               "http://127.0.0.1:8080",
		"http://0x7f.0.0.1:8080":          "http://127.0.0.1:8080",
		"http://[::1]:8080":               "http://[::1]:8080",
		"http://[::ffff:127.0.0.1]:8080":  "http://127.0.0.1:8080",
		"http://app.localhost:5173":       "http://app.localhost:5173",
		"https://localhost:8443":          "https://localhost:8443",
		"http://0.0.0.0:8000":             "http://0.0.0.0:8000",
		"http://127.0.0.1:08080/path?q=1": "http://127.0.0.1:8080",
	}
	for raw, want := range accept {
		target, err := ParseTarget(raw)
		if err != nil || target.String() != want {
			t.Errorf("ParseTarget(%q) = %v, %v; want %s", raw, target, err, want)
		}
	}
	reject := map[string]string{
		"http://localhost/x":            "localhost URL needs an explicit port: http://localhost:PORT/path",
		"http://localhost":              "explicit port",
		"http://example.com:8080/":      "not a loopback",
		"http://[::2]:8080/":            "not a loopback",
		"http://128.0.0.1:80/":          "not a loopback",
		"http://localhost:0/":           "invalid port",
		"http://localhost:70000/":       "invalid port",
		"ftp://localhost:21/":           "only http",
		"http://u:p@localhost:80/":      "user name",
		"http://localhost.evil.com:80/": "not a loopback",
	}
	for raw, want := range reject {
		if _, err := ParseTarget(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseTarget(%q) error %v; want %q", raw, err, want)
		}
	}
}

func TestAttachUnknownStream(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := Attach(a, "nope", ""); err == nil {
		t.Fatal("an unknown stream id was accepted")
	}
}
