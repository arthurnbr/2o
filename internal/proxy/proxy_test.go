package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func startBridge(t *testing.T, remoteURL string) (string, func()) {
	t.Helper()
	local, remote := net.Pipe()
	address := make(chan string, 1)
	localDone := make(chan error, 1)
	remoteDone := make(chan error, 1)
	go func() {
		localDone <- Local(local, remoteURL, func(s string) error {
			address <- s
			return nil
		})
	}()
	go func() { remoteDone <- Remote(remote, remoteURL) }()
	select {
	case s := <-address:
		return s, func() {
			remote.Close()
			select {
			case err := <-localDone:
				if err != nil {
					t.Errorf("local shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("local listener did not close when connection closed")
			}
			select {
			case <-remoteDone:
			case <-time.After(3 * time.Second):
				t.Error("remote worker did not stop")
			}
			u, err := url.Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", u.Host, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Error("listener remained open after connection closed")
			}
		}
	case err := <-localDone:
		t.Fatalf("local failed before opening: %v", err)
	case <-time.After(3 * time.Second):
		local.Close()
		remote.Close()
		t.Fatal("local listener did not start")
	}
	return "", nil
}

func TestBridgeRequestsAndRedirects(t *testing.T) {
	var serviceURL string
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, serviceURL+"/landed?through=tunnel", http.StatusFound)
			return
		}
		if r.URL.Path == "/redirect-relative" {
			http.Redirect(w, r, "//localhost:"+strings.TrimPrefix(serviceURL, "http://127.0.0.1:")+"/landed?through=alias", http.StatusFound)
			return
		}
		if r.URL.Path == "/redirect-other" {
			http.Redirect(w, r, "http://127.0.0.1:1/not-the-service", http.StatusFound)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("request body: %v", err)
			return
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "%s %s %s %s", r.Method, r.URL.RequestURI(), r.Host, payload)
	}))
	defer service.Close()
	serviceURL = service.URL

	localURL, closeBridge := startBridge(t, serviceURL+"/initial%20path?before=1")
	defer closeBridge()
	parsed, err := url.Parse(localURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "127.0.0.1" || parsed.EscapedPath() != "/initial%20path" || parsed.RawQuery != "before=1" {
		t.Fatalf("unexpected browser URL: %s", localURL)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, localURL, strings.NewReader("request payload"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := "POST /initial%20path?before=1 " + parsedRemoteHost(t, serviceURL) + " request payload"
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Upstream") != "yes" || string(body) != want {
		t.Fatalf("status=%d headers=%v body=%q; want %q", response.StatusCode, response.Header, body, want)
	}

	// A remote absolute Location must be rewritten to the local listener;
	// otherwise this request would target the desktop's own localhost.
	response, err = client.Get("http://" + parsed.Host + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want = "GET /landed?through=tunnel " + parsedRemoteHost(t, serviceURL) + " "
	if response.StatusCode != http.StatusCreated || string(body) != want {
		t.Fatalf("redirect status=%d body=%q; want %q", response.StatusCode, body, want)
	}
	response, err = client.Get("http://" + parsed.Host + "/redirect-relative")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want = "GET /landed?through=alias " + parsedRemoteHost(t, serviceURL) + " "
	if response.StatusCode != http.StatusCreated || string(body) != want {
		t.Fatalf("alias redirect status=%d body=%q; want %q", response.StatusCode, body, want)
	}

	response, err = client.Get("http://" + parsed.Host + "/redirect-other")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("other loopback redirect reached desktop: status=%d", response.StatusCode)
	}
}

func TestIPv6RemoteAndSynchronousOpen(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s %s", r.Method, r.URL.RequestURI(), r.Host, body)
	}))
	service.Listener = listener
	service.Start()
	defer service.Close()

	local, remote := net.Pipe()
	localDone := make(chan error, 1)
	remoteDone := make(chan error, 1)
	opened := make(chan struct{})
	go func() { remoteDone <- Remote(remote, service.URL) }()
	go func() {
		localDone <- Local(local, service.URL+"/ipv6?query=yes", func(address string) error {
			defer close(opened)
			req, err := http.NewRequest(http.MethodPost, address, strings.NewReader("body"))
			if err != nil {
				return err
			}
			client := &http.Client{Timeout: 3 * time.Second}
			response, err := client.Do(req)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			want := "POST /ipv6?query=yes " + parsedRemoteHost(t, service.URL) + " body"
			if response.StatusCode != http.StatusOK || string(body) != want {
				return fmt.Errorf("status=%d body=%q, want %q", response.StatusCode, body, want)
			}
			return nil
		})
	}()
	select {
	case <-opened:
	case <-time.After(4 * time.Second):
		remote.Close()
		t.Fatal("synchronous opening blocked")
	}
	remote.Close()
	select {
	case err := <-localDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("synchronous opening blocked")
	}
	select {
	case <-remoteDone:
	case <-time.After(3 * time.Second):
		t.Fatal("remote worker did not stop")
	}
}

func parsedRemoteHost(t *testing.T, remoteURL string) string {
	t.Helper()
	u, err := url.Parse(remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestConcurrentRequestsRemainAligned(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s:%s:%s", r.URL.Query().Get("id"), r.Header.Get("X-ID"), body)
	}))
	defer service.Close()
	localURL, closeBridge := startBridge(t, service.URL)
	defer closeBridge()
	client := &http.Client{Timeout: 8 * time.Second}
	var workers sync.WaitGroup
	for i := range 24 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			value := fmt.Sprint(i)
			req, err := http.NewRequest(http.MethodPost, localURL+"/item?id="+value, strings.NewReader(value))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("X-ID", value)
			res, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode != http.StatusOK || string(body) != value+":"+value+":"+value {
				t.Errorf("request %s: status=%d body=%q err=%v", value, res.StatusCode, body, err)
			}
		}(i)
	}
	workers.Wait()
}

func TestRemoteCannotSelectAnotherOrigin(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Host, r.URL.RequestURI())
	}))
	defer service.Close()
	local, remote := net.Pipe()
	finished := make(chan error, 1)
	go func() { finished <- Remote(remote, service.URL) }()
	_, err := io.WriteString(local, "GET http://outside.example/inside?x=1 HTTP/1.1\r\nHost: outside.example\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(local), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := parsedRemoteHost(t, service.URL) + " /inside?x=1"
	if response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("status=%d body=%q, want %q", response.StatusCode, body, want)
	}
	local.Close()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("remote worker did not stop")
	}
}

func TestRejectNonLoopbackTargets(t *testing.T) {
	for _, target := range []string{"http://example.com:8080/", "https://127.0.0.1:8080/", "http://127.0.0.1/", "http://[::2]:8080/", "http://localhost:0/"} {
		t.Run(target, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			if err := Local(left, target, func(string) error { t.Fatal("opened invalid target"); return nil }); err == nil {
				t.Error("Local accepted invalid target")
			}
			if err := Remote(right, target); err == nil {
				t.Error("Remote accepted invalid target")
			}
		})
	}
}
