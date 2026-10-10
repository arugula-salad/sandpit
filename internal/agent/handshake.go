package agent

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// Over vsock only the host can reach the agent, so it takes every stream it is
// handed. Over TCP (a sandbox that is a Kubernetes pod) anything on the pod
// network could, so each stream starts the way a vsock stream does from
// Firecracker's side, with one line and its answer, and that line carries a
// token the agent was started with:
//
//	client: "AUTH <token>\n"
//	agent:  "OK\n"
//
// after which the stream is the agent's HTTP API as it is over vsock. A wrong
// token, or none within handshakeTimeout, gets the stream closed unanswered.

const handshakeTimeout = 10 * time.Second

// TokenListener is ln, accepting only the streams that open with token.
func TokenListener(ln net.Listener, token string) net.Listener {
	tl := &tokenListener{Listener: ln, token: []byte(token), conns: make(chan net.Conn), done: make(chan struct{})}
	go tl.run()
	return tl
}

type tokenListener struct {
	net.Listener
	token []byte
	conns chan net.Conn
	done  chan struct{}
	err   error // the inner Accept's, once done is closed
}

// run accepts from the inner listener and checks each stream on a goroutine of
// its own, so a client that never sends its line holds up nobody else.
func (l *tokenListener) run() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.err = err
			close(l.done)
			return
		}
		go func() {
			if err := l.check(c); err != nil {
				log.Printf("agent handshake from %s: %v", c.RemoteAddr(), err)
				c.Close()
				return
			}
			select {
			case l.conns <- c:
			case <-l.done:
				c.Close()
			}
		}()
	}
}

func (l *tokenListener) check(c net.Conn) error {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	line, err := readLine(c)
	if err != nil {
		return err
	}
	got, ok := strings.CutPrefix(line, "AUTH ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), l.token) != 1 {
		return errors.New("bad token")
	}
	if _, err := c.Write([]byte("OK\n")); err != nil {
		return err
	}
	return c.SetDeadline(time.Time{})
}

func (l *tokenListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, l.err
	}
}

// Handshake is the client's half: it sends token on c and waits for the
// agent's OK, within c's deadline (the caller's to set).
func Handshake(c net.Conn, token string) error {
	if _, err := fmt.Fprintf(c, "AUTH %s\n", token); err != nil {
		return err
	}
	line, err := readLine(c)
	if err != nil {
		return fmt.Errorf("agent handshake: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("agent handshake: %q", line)
	}
	return nil
}

// readLine reads one short line byte by byte: anything after the newline
// belongs to the HTTP that follows.
func readLine(c net.Conn) (string, error) {
	var b [1]byte
	var line []byte
	for len(line) < 256 {
		if _, err := c.Read(b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("line too long")
}
