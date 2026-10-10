package agent

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// An unclaimed agent behind an upgrade: the first CLAIM sets the token, AUTH
// with it opens later streams, and a wrong token or a second CLAIM is refused.
func TestUpgradeClaim(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := ClaimListener(UpgradeListener(inner))
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { // echo one line: the agent's API, as far as this test cares
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("echo " + line))
			}()
		}
	}()

	open := func(t *testing.T) net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		uc, err := DialUpgrade(c, "actor")
		if err != nil {
			t.Fatal(err)
		}
		return uc
	}
	roundTrip := func(t *testing.T, c net.Conn) {
		t.Helper()
		c.Write([]byte("hi\n"))
		got, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || got != "echo hi\n" {
			t.Fatalf("after the handshake: %q, %v", got, err)
		}
	}

	c := open(t)
	if err := Claim(c, "s3cret"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	roundTrip(t, c)
	c.Close()

	c = open(t)
	if err := Handshake(c, "s3cret"); err != nil {
		t.Fatalf("auth with the claimed token: %v", err)
	}
	roundTrip(t, c)
	c.Close()

	c = open(t)
	if err := Handshake(c, "wrong"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
	c.Close()

	c = open(t)
	if err := Claim(c, "another"); err == nil {
		t.Fatal("a second claim was accepted")
	}
	c.Close()

	// No upgrade at all: refused before any handshake.
	raw, _ := net.Dial("tcp", inner.Addr().String())
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	raw.Write([]byte("AUTH s3cret\n"))
	if b, _ := bufio.NewReader(raw).ReadString('\n'); b == "OK\n" {
		t.Fatal("a stream without the upgrade got through")
	}
	raw.Close()
}
