package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenBridgeNeedsExplicitPort(t *testing.T) {
	startDesktop(t)
	err := openBridge("http://localhost/app")
	if err == nil || err.Error() != "localhost URL needs an explicit port: http://localhost:PORT/path" || exitCode(err) != exitUsage {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
}

// A browser that cannot be opened must fail the remote command, before any
// bridge is acknowledged, and leave no listener behind.
func TestOpenBridgeReportsBrowserFailure(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("service contacted although the browser never opened")
	}))
	defer service.Close()
	startDesktop(t)
	var bridgeURL string
	old := openBrowser
	openBrowser = func(u string) error { bridgeURL = u; return errors.New("xdg-open: no method available") }
	t.Cleanup(func() { openBrowser = old })

	err := openBridge(service.URL + "/")
	if err == nil || !strings.Contains(err.Error(), "could not open a browser") || exitCode(err) == 0 {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
	if bridgeURL == "" {
		t.Fatal("browser was never asked")
	}
	host := strings.TrimPrefix(bridgeURL, "http://")
	host = host[:strings.IndexByte(host, '/')]
	if c, e := net.Dial("tcp", host); e == nil {
		c.Close()
		t.Fatal("bridge listener survived the failed open")
	}
}

// Full path: browser → desktop bridge → authenticated proxy-stream
// connections opened by the server → service; the remote command ends with
// an error when the desktop session goes away.
func TestOpenBridgeServesThroughAuthenticatedStreams(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "served "+r.URL.RequestURI())
	}))
	defer service.Close()
	d := startDesktop(t)
	pages := make(chan string, 1)
	old := openBrowser
	openBrowser = func(u string) error {
		go func() {
			jar, _ := cookiejar.New(nil)
			res, err := (&http.Client{Jar: jar, Timeout: 5 * time.Second}).Get(u)
			if err != nil {
				pages <- err.Error()
				return
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			pages <- string(body)
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = old })

	done := make(chan error, 1)
	go func() { done <- openBridge(service.URL + "/app?x=1") }()
	select {
	case page := <-pages:
		if page != "served /app?x=1" {
			t.Fatalf("browser got %q", page)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("page never arrived")
	}
	d.release() // the `2o ssh` session ends
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed the bridge") || exitCode(err) != exitNoTunnel {
			t.Fatalf("got %v (exit %d)", err, exitCode(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("2o open kept running after the desktop session ended")
	}
}
