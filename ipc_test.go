package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Commands that need no gateway run with a nil client.
func serveWithoutGateway(conn net.Conn) { handleConn(conn, nil) }

// forward sends argv to the daemon on addr and returns its stdout and stderr.
func forward(t *testing.T, addr string, argv ...string) (stdout, stderr string, forwarded bool, err error) {
	t.Helper()
	var o, e bytes.Buffer
	forwarded, err = forwardTo(addr, argv, &o, &e)
	return o.String(), e.String(), forwarded, err
}

// The gateway's per-call log line is dropped, even split across writes.
func TestDropLines(t *testing.T) {
	var out bytes.Buffer
	d := &dropLines{w: &out, drop: gatewayLogNoise}
	for _, chunk := range []string{
		"12:00 [INFO] Gateway started\n12:00 [INFO] mcp: Processing req",
		"uest of type CallToolRequest\n12:01 [ERROR] Timeout",
		" waiting for ESP32\n",
	} {
		if n, err := d.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if want := "12:00 [INFO] Gateway started\n12:01 [ERROR] Timeout waiting for ESP32\n"; out.String() != want {
		t.Errorf("log = %q, want %q", out.String(), want)
	}

	// The last words of a crash, without a newline, are written on Flush.
	_, _ = d.Write([]byte("Fatal Python error: Aborted"))
	d.Flush()
	if !strings.HasSuffix(out.String(), "Fatal Python error: Aborted") {
		t.Errorf("after Flush: log ends %q", out.String()[max(0, out.Len()-40):])
	}

	// Output that never ends a line is written once it passes the cap.
	out.Reset()
	_, _ = d.Write(bytes.Repeat([]byte("\rdownloading"), dropLinesMax/10))
	if out.Len() == 0 {
		t.Error("over the cap without a newline: nothing written")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A log that cannot be written must not stop the gateway's stderr copier.
func TestDropLinesSwallowsWriteErrors(t *testing.T) {
	d := &dropLines{w: failingWriter{}, drop: gatewayLogNoise}
	if n, err := d.Write([]byte("line\n")); err != nil || n != 5 {
		t.Errorf("Write = %d, %v; want 5, nil", n, err)
	}
}

func TestForwardToDaemonSuccess(t *testing.T) {
	addr := listenLocal(t, serveWithoutGateway)
	stdout, stderr, forwarded, err := forward(t, addr, "help")
	if !forwarded || err != nil {
		t.Fatalf("forwardTo = (%v, %v), want (true, nil)", forwarded, err)
	}
	if !strings.Contains(stdout, "commands (device stays connected") {
		t.Errorf("stdout = %q, want the help text", stdout)
	}
	if strings.Contains(stdout, "\x1e") || stderr != "" {
		t.Errorf("protocol lines leaked: stdout %q, stderr %q", stdout, stderr)
	}
}

func TestForwardToDaemonCommandError(t *testing.T) {
	addr := listenLocal(t, serveWithoutGateway)
	// The argument with a space also checks that quoting survives the trip.
	_, _, forwarded, err := forward(t, addr, "sleep", "1 2")
	if !forwarded {
		t.Fatal("forwarded = false, want true")
	}
	if err == nil || err.Error() != `sleep: invalid ms "1 2"` {
		t.Errorf("err = %v, want the daemon's error", err)
	}
}

// What a command writes to errOut reaches the client's stderr, as in one-shot
// mode; results stay on stdout.
func TestForwardToSplitsStdoutAndStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "s.txt")
	if err := os.WriteFile(script, []byte("sleep x\nhelp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := listenLocal(t, serveWithoutGateway)
	stdout, stderr, _, err := forward(t, addr, "source", script)
	if err == nil || !strings.Contains(err.Error(), "1 line(s) failed") {
		t.Errorf("err = %v, want the failed-line count", err)
	}
	if !strings.Contains(stdout, "commands (device stays connected") || strings.Contains(stdout, "invalid ms") {
		t.Errorf("stdout = %q, want only the help text", stdout)
	}
	if stderr != "error: sleep: invalid ms \"x\"\n" {
		t.Errorf("stderr = %q, want the script error", stderr)
	}
}

func TestStderrLinesMarksEveryLine(t *testing.T) {
	var buf bytes.Buffer
	s := &stderrLines{w: &buf}
	for _, part := range []string{"\n23 tools\n", "partial", " line\nnext"} {
		if _, err := s.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	s.endLine()
	p := ipcStderrPrefix
	if want := p + "\n" + p + "23 tools\n" + p + "partial line\n" + p + "next\n"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// A long command (here sleep, standing in for listen or say) must not hold up
// another IPC command, and each reply carries only its own output.
func TestLongCommandDoesNotBlockOthers(t *testing.T) {
	addr := listenLocal(t, serveWithoutGateway)
	longDone := make(chan error, 1)
	var longOut, longErr bytes.Buffer
	go func() {
		_, err := forwardTo(addr, []string{"sleep", "1500"}, &longOut, &longErr)
		longDone <- err
	}()
	time.Sleep(200 * time.Millisecond) // let the daemon start the sleep

	stdout, _, _, err := forward(t, addr, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	// Judge by order, not by a time limit: help must finish while sleep runs.
	select {
	case <-longDone:
		t.Error("help finished after sleep 1500; it waited for the long command")
	default:
	}
	if !strings.Contains(stdout, "commands (device stays connected") {
		t.Errorf("help output = %q", stdout)
	}

	if err := <-longDone; err != nil {
		t.Errorf("sleep: %v", err)
	}
	if longOut.Len() != 0 || longErr.Len() != 0 {
		t.Errorf("sleep's reply got output %q / %q; the help text leaked into it?", longOut.String(), longErr.String())
	}
}

// Commands running at the same time write only to their own cmdEnv.
func TestConcurrentCommandsKeepOutputSeparate(t *testing.T) {
	const n = 8
	bufs := make([]bytes.Buffer, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			e := cmdEnv{out: &bufs[i], errOut: &bufs[i]}
			if _, err := runLine("help", e); err != nil {
				t.Errorf("help: %v", err)
			}
		})
	}
	wg.Wait()
	var want bytes.Buffer
	replHelp(&want)
	for i := range bufs {
		if bufs[i].String() != want.String() {
			t.Errorf("output %d = %q, want exactly one help text", i, bufs[i].String())
		}
	}
}

func TestForwardToNoDaemon(t *testing.T) {
	_, _, forwarded, err := forward(t, closedLocalAddr(t), "status")
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
	stdout, _, forwarded, err := forward(t, addr, "status")
	if !forwarded {
		t.Fatal("forwarded = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "before reporting a result") {
		t.Errorf("err = %v, want a failure for the missing status line", err)
	}
	if stdout != "partial output\n" {
		t.Errorf("stdout = %q, want the partial output", stdout)
	}
}

func TestMultilineErrorStaysOneStatusLine(t *testing.T) {
	addr := listenLocal(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("output\n"))
		writeIPCStatus(conn, errors.New("first line\nsecond line\r\nthird\rfourth"))
	})
	stdout, _, _, err := forward(t, addr, "status")
	if want := "first line second line third fourth"; err == nil || err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if stdout != "output\n" {
		t.Errorf("stdout = %q, want %q", stdout, "output\n")
	}
}

func TestWriteIPCStatusOK(t *testing.T) {
	var buf bytes.Buffer
	writeIPCStatus(&buf, nil)
	if got, want := buf.String(), ipcStatusPrefix+"ok\n"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}
