package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

// Commands that need no gateway run with a nil client.
func serveWithoutGateway(conn net.Conn) { handleConn(conn, nil) }

func TestForwardToDaemonSuccess(t *testing.T) {
	addr := listenLocal(t, serveWithoutGateway)
	var buf bytes.Buffer
	forwarded, err := forwardTo(addr, []string{"help"}, &buf)
	if !forwarded || err != nil {
		t.Fatalf("forwardTo = (%v, %v), want (true, nil)", forwarded, err)
	}
	if !strings.Contains(buf.String(), "commands (device stays connected") {
		t.Errorf("output = %q, want the help text", buf.String())
	}
	if strings.Contains(buf.String(), ipcStatusPrefix) {
		t.Errorf("the status line leaked into the output: %q", buf.String())
	}
}

func TestForwardToDaemonCommandError(t *testing.T) {
	addr := listenLocal(t, serveWithoutGateway)
	var buf bytes.Buffer
	// The argument with a space also checks that quoting survives the trip.
	forwarded, err := forwardTo(addr, []string{"sleep", "1 2"}, &buf)
	if !forwarded {
		t.Fatal("forwarded = false, want true")
	}
	if err == nil || err.Error() != `sleep: invalid ms "1 2"` {
		t.Errorf("err = %v, want the daemon's error", err)
	}
}

func TestForwardToNoDaemon(t *testing.T) {
	forwarded, err := forwardTo(closedLocalAddr(t), []string{"status"}, &bytes.Buffer{})
	if forwarded || err != nil {
		t.Errorf("forwardTo = (%v, %v), want (false, nil) so the caller falls back", forwarded, err)
	}
}

func TestForwardToMissingStatusLine(t *testing.T) {
	addr := listenLocal(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("partial output\n")) // then the daemon dies
	})
	var buf bytes.Buffer
	forwarded, err := forwardTo(addr, []string{"status"}, &buf)
	if !forwarded {
		t.Fatal("forwarded = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "before reporting a result") {
		t.Errorf("err = %v, want a failure for the missing status line", err)
	}
	if buf.String() != "partial output\n" {
		t.Errorf("output = %q, want the partial output", buf.String())
	}
}

func TestMultilineErrorStaysOneStatusLine(t *testing.T) {
	addr := listenLocal(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("output\n"))
		writeIPCStatus(conn, errors.New("first line\nsecond line\r\nthird\rfourth"))
	})
	var buf bytes.Buffer
	_, err := forwardTo(addr, []string{"status"}, &buf)
	if want := "first line second line third fourth"; err == nil || err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if buf.String() != "output\n" {
		t.Errorf("output = %q, want %q", buf.String(), "output\n")
	}
}

func TestWriteIPCStatusOK(t *testing.T) {
	var buf bytes.Buffer
	writeIPCStatus(&buf, nil)
	if got, want := buf.String(), ipcStatusPrefix+"ok\n"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}
