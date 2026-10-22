package main

import (
	"errors"
	"net"
	"net/url"

	"github.com/arthurnbr/2o/internal/desktop"
	"github.com/arthurnbr/2o/internal/proxy"
	"github.com/arthurnbr/2o/internal/ui"
	"github.com/arthurnbr/2o/internal/wire"
)

// openBrowser is replaced in tests.
var openBrowser = desktop.Open

// openBridge runs on the server: `2o open http://localhost:PORT/path` shows a
// service of the server in the desktop browser until Ctrl-C. Everything that
// can fail (URL, browser) is reported before the bridge is acknowledged.
func openBridge(target string) error {
	t, err := proxy.ParseTarget(target)
	if err != nil {
		return fail("%s", err.Error()).exit(exitUsage)
	}
	s := ui.Start("opening %s on the desktop", t)
	c, err := request(wire.Message{Op: "proxy", URL: target})
	if err != nil {
		s.Fail()
		return err
	}
	defer c.Close()
	ack, err := wire.Read(c)
	if err != nil {
		s.Fail()
		return fail("the desktop closed the bridge before it was ready").because(err).
			hint("check the desktop daemon log (~/.cache/2o/daemon.log on the desktop)").exit(exitNoTunnel)
	}
	if err = remoteErr(ack); err != nil {
		s.Fail()
		return err
	}
	where := ack.Desktop
	if where == "" {
		where = "the desktop"
	}
	bridge := ack.URL
	if u, e := url.Parse(ack.URL); e == nil {
		bridge = u.Host
	}
	s.DoneAs("%s → opened on %s (bridge %s) — Ctrl-C to close", t, ui.Bold(where), bridge)
	connect := func(id, failure string) (net.Conn, error) {
		return request(wire.Message{Op: "proxy-stream", Name: id, Body: failure})
	}
	err = proxy.Remote(c, t, connect, func(e error) { ui.Warn("%s", e) })
	if errors.Is(err, proxy.ErrBridgeClosed) {
		return fail("the desktop closed the bridge to %s", t).
			hint("run the same 2o open command again; if it keeps closing, check the desktop daemon").exit(exitNoTunnel)
	}
	return err
}

// serveProxy is the desktop end of a bridge: a private loopback listener,
// opened in the browser, that lives as long as the control connection c.
func serveProxy(c net.Conn, m wire.Message) {
	if _, err := proxy.ParseTarget(m.URL); err != nil {
		replyErr(c, fail("%s", err.Error()).exit(exitUsage))
		return
	}
	b, err := proxy.Listen(c, m.URL)
	if err != nil {
		replyErr(c, fail("the desktop could not start the bridge").because(err).hint("%v", err))
		return
	}
	// Close also closes c: answer first.
	if err = openBrowser(b.StartURL()); err != nil {
		replyErr(c, fail("the desktop could not open a browser").because(err).
			hint("%v", err).hint("check the desktop's default browser (xdg-settings get default-web-browser), or run 2o doctor"))
		b.Close()
		return
	}
	if err = wire.Write(c, wire.Message{URL: b.URL(), Desktop: desktopLabel()}); err != nil {
		b.Close()
		return
	}
	_ = b.Serve()
}

// serveProxyStream receives one tunnel connection the server opened for a
// bridge stream and holds it until the stream ends. Nothing is written back:
// the connection carries raw HTTP from here on.
func serveProxyStream(c net.Conn, m wire.Message) {
	_ = proxy.Attach(c, m.Name, m.Body)
}
