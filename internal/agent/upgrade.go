package agent

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// UpgradeListener is ln, accepting each stream only after answering the HTTP
// upgrade it opens with, so the stream can cross a proxy that carries HTTP
// alone: Agent Substrate's router, which passes `Upgrade: websocket` through
// (and nothing after the 101 is looked at) but refuses other upgrades. The
// WebSocket name is only the router's ticket: what follows the 101 is the
// agent's own protocol, unframed, as on any other listener.
func UpgradeListener(ln net.Listener) net.Listener {
	ul := &upgradeListener{Listener: ln, conns: make(chan net.Conn), done: make(chan struct{})}
	go ul.run()
	return ul
}

type upgradeListener struct {
	net.Listener
	conns chan net.Conn
	done  chan struct{}
	err   error
}

func (l *upgradeListener) run() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.err = err
			close(l.done)
			return
		}
		go func() {
			uc, err := acceptUpgrade(c)
			if err != nil {
				c.Close()
				return
			}
			select {
			case l.conns <- uc:
			case <-l.done:
				c.Close()
			}
		}()
	}
}

func (l *upgradeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, l.err
	}
}

func acceptUpgrade(c net.Conn) (net.Conn, error) {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		c.Write([]byte("HTTP/1.1 426 Upgrade Required\r\nUpgrade: websocket\r\nContent-Length: 0\r\n\r\n"))
		return nil, errors.New("not an upgrade")
	}
	if _, err := c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// DialUpgrade is the client's half: it asks for the upgrade on c, which leads
// to the agent's listener, and returns the stream after the 101.
func DialUpgrade(c net.Conn, host string) (net.Conn, error) {
	if _, err := c.Write([]byte("GET /sandpit-agent HTTP/1.1\r\nHost: " + host +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: c2FuZHBpdC1hZ2VudA==\r\n\r\n")); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, errors.New("agent upgrade: " + resp.Status)
	}
	return &bufConn{Conn: c, r: br}, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
