// Package proxy carries an HTTP service bound to a remote loopback address over
// a single full-duplex connection. One request/response exchange uses the
// connection at a time; local browsers can still make concurrent requests.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// origin accepts only explicit loopback HTTP ports. The remote host is never
// inferred from the SSH peer: that peer may be reached through a jump host.
func origin(remoteURL string) (*url.URL, error) {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Host == "" {
		return nil, fmt.Errorf("proxy target must be a loopback http URL: %q", remoteURL)
	}
	if !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("proxy target is not loopback: %q", u.Host)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("proxy target needs a valid explicit port: %q", u.Host)
	}
	return u, nil
}

func loopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// Local exposes remoteURL on a temporary local loopback port and calls open
// with its browser URL once the listener is serving. It runs until the remote
// connection closes; closing the connection also closes the local listener.
func Local(conn net.Conn, remoteURL string, open func(string) error) error {
	target, err := origin(remoteURL)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	defer conn.Close()
	localURL := *target
	localURL.Host = listener.Addr().String()
	if localURL.Path == "" {
		localURL.Path = "/"
	}

	// A dedicated reader detects EOF even while no browser requests are active.
	// io.Pipe passes response bytes on without buffering them; the mutex below
	// makes sure a response is completely consumed before the next request.
	responses, incoming := io.Pipe()
	defer responses.Close()
	go func() {
		_, copyErr := io.Copy(incoming, conn)
		incoming.CloseWithError(copyErr)
		listener.Close()
	}()

	var exchange sync.Mutex
	reader := bufio.NewReader(responses)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchange.Lock()
		defer exchange.Unlock()

		forward := *r
		forward.URL = new(url.URL)
		*forward.URL = *r.URL
		forward.URL.Scheme = ""
		forward.URL.Host = ""
		forward.URL.User = nil
		forward.URL.Fragment = ""
		forward.Host = target.Host
		forward.RequestURI = ""
		if err := forward.Write(conn); err != nil {
			http.Error(w, "remote proxy unavailable", http.StatusBadGateway)
			return
		}
		response, err := http.ReadResponse(reader, &forward)
		if err != nil {
			http.Error(w, "remote proxy unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		// Absolute and scheme-relative redirects to remote loopback cannot
		// be allowed to hit the desktop's own loopback address.
		if location := response.Header.Get("Location"); location != "" {
			redirect, parseErr := url.Parse(location)
			if parseErr == nil && redirect.Host != "" && loopbackHost(redirect.Hostname()) {
				if (redirect.Scheme != "" && redirect.Scheme != "http") || redirect.Port() != target.Port() {
					io.Copy(io.Discard, response.Body)
					http.Error(w, "redirect outside remote proxy origin", http.StatusBadGateway)
					return
				}
				redirect.Scheme = "http"
				redirect.Host = localURL.Host
				response.Header.Set("Location", redirect.String())
			}
		}
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, writeErr := io.Copy(w, response.Body)
		if writeErr != nil {
			// Keep the next exchange aligned if a browser disconnects early.
			io.Copy(io.Discard, response.Body)
		}
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	if err := open(localURL.String()); err != nil {
		return err
	}
	err = <-served
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Remote processes serialized requests arriving from Local, contacting only
// the service specified by remoteURL. Transport.RoundTrip never follows HTTP
// redirects, so Location headers are returned to the local browser unchanged.
func Remote(conn net.Conn, remoteURL string) error {
	target, err := origin(remoteURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
		},
	}
	defer transport.CloseIdleConnections()
	reader := bufio.NewReader(conn)
	for {
		r, err := http.ReadRequest(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Never trust an absolute-form URL or Host sent over the tunnel to
		// choose a destination. Keep only its path and query.
		r.URL.Scheme = "http"
		r.URL.Host = target.Host
		r.URL.Opaque = ""
		r.URL.User = nil
		r.URL.Fragment = ""
		r.Host = target.Host
		r.RequestURI = ""
		response, err := transport.RoundTrip(r)
		if err != nil {
			r.Body.Close()
			failure := &http.Response{
				StatusCode:    http.StatusBadGateway,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("remote proxy unavailable\n")),
				ContentLength: int64(len("remote proxy unavailable\n")),
			}
			writeErr := failure.Write(conn)
			failure.Body.Close()
			if writeErr != nil {
				return writeErr
			}
			return err
		}
		writeErr := response.Write(conn)
		response.Body.Close()
		if writeErr != nil {
			return writeErr
		}
	}
}
