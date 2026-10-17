package main

import (
	"net"
	"testing"
)

// fakeHome points stackchanDir() (~/.stackchan) at a temp dir.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir on Linux
	t.Setenv("USERPROFILE", home) // and on Windows
	return home
}

// listenLocal opens a listener on a free loopback port (not the daemon's
// fixed 8770, which a running daemon may hold) and serves each connection
// with handle until the test ends.
func listenLocal(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return ln.Addr().String()
}

// closedLocalAddr is a loopback address where nothing listens.
func closedLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
