package main

// Stage-3 daemon: hold one gateway (and the device connection) resident, react
// to touch gestures, and accept commands over a localhost IPC so one-shot
// `stackchan-cli <cmd>` invocations forward to it (fast, no port conflict)
// instead of each spawning its own gateway.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"stackchan-cli/internal/mcp"
)

const ipcAddr = "127.0.0.1:8770"

// execMu serializes command execution so the shared `out` writer is never
// driven by two jobs at once (a touch reaction vs a forwarded IPC command).
var execMu sync.Mutex

// withOutput runs fn with command result output routed to w, serialized.
func withOutput(w io.Writer, fn func()) {
	execMu.Lock()
	defer execMu.Unlock()
	prev := out
	out = w
	defer func() { out = prev }()
	fn()
}

func cmdDaemon(args []string) error {
	set, verbose := fs("daemon")
	connectTimeout := set.Int("connect-timeout", 90, "seconds to wait for the device on startup")
	pollMs := set.Int("touch-poll", 300, "touch poll interval in ms (0 disables touch reactions)")
	if err := set.Parse(args); err != nil {
		return err
	}

	if conn, err := net.DialTimeout("tcp", ipcAddr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("a daemon is already running on %s", ipcAddr)
	}
	ln, err := net.Listen("tcp", ipcAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ipcAddr, err)
	}
	defer ln.Close()

	return withClient(*verbose, func(c *mcp.Client) error {
		fmt.Fprintln(os.Stderr, "daemon: gateway up; waiting for device...")
		if err := waitConnected(c, time.Duration(*connectTimeout)*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v (continuing; will react once it connects)\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "daemon: device connected ✓")
		}
		fmt.Fprintf(os.Stderr, "daemon: IPC on %s · touch-poll %dms · forward with `stackchan-cli <cmd>` · Ctrl-C to stop\n", ipcAddr, *pollMs)

		stop := make(chan struct{})
		var stopOnce sync.Once
		shutdown := func() { stopOnce.Do(func() { close(stop); ln.Close() }) }

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		go func() {
			<-sig
			fmt.Fprintln(os.Stderr, "\ndaemon: shutting down...")
			shutdown()
		}()

		if *pollMs > 0 {
			go touchWatch(c, time.Duration(*pollMs)*time.Millisecond, stop)
		}

		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return nil
				default:
					continue
				}
			}
			go handleConn(conn, c)
		}
	})
}

// handleConn reads one command line from an IPC client, runs it against the
// resident gateway, and streams the result back over the connection.
func handleConn(conn net.Conn, c *mcp.Client) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	line = strings.TrimRight(line, "\r\n")
	withOutput(conn, func() {
		if _, err := runLine(line, c); err != nil {
			fmt.Fprintf(conn, "error: %v\n", err)
		}
	})
}

// touchWatch polls get_touch_state and reacts to fresh tap/stroke gestures.
func touchWatch(c *mcp.Client, interval time.Duration, stop <-chan struct{}) {
	prevAge := -1
	lastReact := time.Time{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		res, err := c.CallTool("get_touch_state", map[string]any{})
		if err != nil {
			continue
		}
		var s struct {
			Available bool   `json:"available"`
			LastEvent string `json:"last_event"`
			AgeMs     int    `json:"last_event_age_ms"`
		}
		if json.Unmarshal([]byte(res.Text()), &s) != nil || !s.Available {
			continue
		}
		// A fresh event re-fired when the age counter drops vs the last poll.
		fresh := prevAge >= 0 && s.AgeMs < prevAge && s.LastEvent != "idle"
		prevAge = s.AgeMs
		if !fresh || time.Since(lastReact) < 2*time.Second {
			continue
		}
		lastReact = time.Now()
		reactToGesture(c, s.LastEvent)
	}
}

// reactToGesture runs a little reaction. Output goes to the daemon log (stderr)
// so it doesn't get tangled with forwarded-command output.
func reactToGesture(c *mcp.Client, gesture string) {
	fmt.Fprintf(os.Stderr, "daemon: touch %q -> reacting\n", gesture)
	withOutput(os.Stderr, func() {
		switch gesture {
		case "stroke":
			_, _ = runLine("all-leds --r 255 --g 120 --b 160", c)
			_, _ = runLine(`say --face embarrassed "えへへ、なでられるの好き。"`, c)
			_, _ = runLine("clear-leds", c)
		case "tap":
			_, _ = runLine("all-leds --r 80 --g 180 --b 255", c)
			_, _ = runLine(`say --face happy "わっ、なあに？"`, c)
			_, _ = runLine("clear-leds", c)
		default:
			_, _ = runLine("avatar happy", c)
		}
	})
}

// forwardToDaemon sends argv to a running daemon and copies the reply to stdout.
// Returns (false, nil) when no daemon is listening (caller falls back).
func forwardToDaemon(argv []string) (bool, error) {
	conn, err := net.DialTimeout("tcp", ipcAddr, 250*time.Millisecond)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", quoteArgs(argv)); err != nil {
		return true, err
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_, err = io.Copy(os.Stdout, conn)
	return true, err
}

// quoteArgs rebuilds a command line, quoting tokens that contain whitespace so
// the daemon's tokenizer reassembles them correctly.
func quoteArgs(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t") {
			parts[i] = `"` + a + `"`
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
