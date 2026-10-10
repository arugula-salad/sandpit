package agent

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestTokenListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := TokenListener(inner, "s3cret")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello"))
			c.Close()
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}

	c := dial()
	if err := Handshake(c, "s3cret"); err != nil {
		t.Fatalf("right token: %v", err)
	}
	if b, _ := io.ReadAll(c); string(b) != "hello" {
		t.Fatalf("after the handshake: %q", b)
	}

	c = dial()
	if err := Handshake(c, "wrong"); err == nil {
		t.Fatal("wrong token was accepted")
	}

	// A client that never speaks does not hold up the next one.
	silent := dial()
	defer silent.Close()
	c = dial()
	if err := Handshake(c, "s3cret"); err != nil {
		t.Fatalf("behind a silent client: %v", err)
	}
}
