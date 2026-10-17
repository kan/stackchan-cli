package main

// Stage-3 daemon: hold one gateway (and the device connection) resident, react
// to touch gestures, and accept commands over a localhost IPC so one-shot
// `stackchan-cli <cmd>` invocations forward to it (fast, no port conflict)
// instead of each spawning its own gateway.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
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
	prevOut, prevErr := out, errOut
	out, errOut = w, w
	defer func() { out, errOut = prevOut, prevErr }()
	fn()
}

func cmdDaemon(args []string) error {
	set, verbose := fs("daemon")
	connectTimeout := set.Int("connect-timeout", 90, "seconds to wait for the device on startup")
	pollMs := set.Int("touch-poll", 150, "touch poll interval in ms (0 disables touch reactions)")
	reactions := set.String("reactions", filepath.Join(stackchanDir(), "reactions"), "dir of per-gesture reaction scripts (<gesture>.txt)")
	if err := set.Parse(args); err != nil {
		return err
	}
	// Report our own fatal error before flushing, so it reaches the log
	// (main would print it after the timestamp relay is gone).
	flush := timestampStderr()
	err := runDaemon(*verbose, *connectTimeout, *pollMs, *reactions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	flush()
	if err != nil {
		os.Exit(1)
	}
	return nil
}

func runDaemon(verbose bool, connectTimeout, pollMs int, reactions string) error {
	reactionsDir = reactions
	seedReactions(reactionsDir)

	if daemonAvailable() {
		return fmt.Errorf("a daemon is already running on %s", ipcAddr)
	}
	ln, err := net.Listen("tcp", ipcAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ipcAddr, err)
	}
	defer ln.Close()

	// Warm up VOICEVOX in the background so the first reaction can speak.
	go func() {
		if err := ensureVoicevox(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		}
	}()

	return withClient(verbose, func(c *mcp.Client) error {
		// Only log the device connection; IPC is served right away so commands
		// (e.g. status) don't hang while the device is away.
		fmt.Fprintln(os.Stderr, "daemon: gateway up; waiting for device...")
		go func() {
			if err := waitConnected(c, time.Duration(connectTimeout)*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: %v (continuing; will react once it connects)\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "daemon: device connected ✓")
			}
		}()
		fmt.Fprintf(os.Stderr, "daemon: IPC on %s · touch-poll %dms · reactions %s · Ctrl-C to stop\n", ipcAddr, pollMs, reactionsDir)

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

		if pollMs > 0 {
			go touchWatch(c, time.Duration(pollMs)*time.Millisecond, stop)
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
			// The status must stay on one line.
			msg := strings.ReplaceAll(err.Error(), "\n", " ")
			fmt.Fprintf(conn, "%serror %s\n", ipcStatusPrefix, msg)
			return
		}
		fmt.Fprintf(conn, "%sok\n", ipcStatusPrefix)
	})
}

// ipcStatusPrefix marks the last line of a daemon reply, which carries the
// command's outcome ("ok" or "error <msg>") so the client can set its exit code.
const ipcStatusPrefix = "\x1estackchan-ipc:"

// timestampStderr prefixes every stderr line with the time, so the daemon log
// shows when each event happened. It swaps os.Stderr for a pipe, which also
// covers children that inherit os.Stderr. The returned flush restores
// os.Stderr and waits (briefly) until everything written so far is relayed.
func timestampStderr() (flush func()) {
	r, w, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(r) // no line-length limit, unlike bufio.Scanner
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				fmt.Fprintf(orig, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), strings.TrimRight(line, "\r\n"))
			}
			if err != nil {
				return
			}
		}
	}()
	return func() {
		os.Stderr = orig
		_ = w.Close()
		select {
		case <-done:
		case <-time.After(time.Second): // a child may still hold the pipe
		}
	}
}

// touchWatch polls get_touch_state and classifies the gesture by *contact
// duration* (the firmware's own tap/stroke label skews heavily to "stroke"):
// a short cover-and-release is a tap; a longer hold is a stroke. Reacting on
// release (or after a long hold) makes both reliable with a palm-sized touch.
func touchWatch(c *mcp.Client, interval time.Duration, stop <-chan struct{}) {
	const tapMax = 1000 * time.Millisecond     // release within this => tap
	const strokeHold = 1800 * time.Millisecond // held this long => stroke mid-contact
	const cooldown = 2 * time.Second

	var touchStart time.Time // zero = not currently touching
	reacted := false         // already reacted for the current contact
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
			Available bool `json:"available"`
			Zone0     bool `json:"zone0"`
			Zone1     bool `json:"zone1"`
			Zone2     bool `json:"zone2"`
			Raw       int  `json:"raw"`
		}
		if json.Unmarshal([]byte(res.Text()), &s) != nil || !s.Available {
			continue
		}
		active := s.Raw > 0 || s.Zone0 || s.Zone1 || s.Zone2
		now := time.Now()

		switch {
		case active && touchStart.IsZero(): // rising edge
			touchStart = now
			reacted = false
		case active && !reacted && now.Sub(touchStart) >= strokeHold: // long hold
			if now.Sub(lastReact) >= cooldown {
				lastReact = now
				reacted = true
				reactToGesture(c, "stroke")
			}
		case !active && !touchStart.IsZero(): // falling edge (release)
			dur := now.Sub(touchStart)
			touchStart = time.Time{}
			if !reacted && now.Sub(lastReact) >= cooldown {
				lastReact = now
				if dur <= tapMax {
					reactToGesture(c, "tap")
				} else {
					reactToGesture(c, "stroke")
				}
			}
			reacted = false
		}
	}
}

// reactionsDir holds per-gesture reaction scripts (<gesture>.txt). Set by cmdDaemon.
var reactionsDir string

// defaultReactions seed the reaction-script files on first run so they can be
// edited without rebuilding. They mirror the built-in fallback behavior.
var defaultReactions = map[string]string{
	"tap": `# Reaction for a quick tap. Any REPL commands work (avatar/mouth/led/say/sleep...).
all-leds --r 80 --g 180 --b 255
say --face happy "わっ、なあに？"
clear-leds
`,
	"stroke": `# Reaction for a stroke / long touch.
all-leds --r 255 --g 120 --b 160
say --face embarrassed "えへへ、なでられるの好き。"
clear-leds
`,
}

func seedReactions(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	for gesture, body := range defaultReactions {
		p := filepath.Join(dir, gesture+".txt")
		if _, err := os.Stat(p); os.IsNotExist(err) {
			_ = os.WriteFile(p, []byte(body), 0o644)
		}
	}
}

// reactToGesture runs the reaction for a gesture: the script file
// reactionsDir/<gesture>.txt if present, else a built-in fallback. Output goes
// to the daemon log (stderr) so it doesn't tangle with forwarded-command output.
func reactToGesture(c *mcp.Client, gesture string) {
	fmt.Fprintf(os.Stderr, "daemon: touch %q -> reacting\n", gesture)
	if reactionsDir != "" {
		if file := filepath.Join(reactionsDir, gesture+".txt"); fileExists(file) {
			withOutput(os.Stderr, func() { _ = doSource([]string{file}, c) })
			return
		}
	}
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

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// daemonAvailable reports whether a daemon is listening on the IPC port.
func daemonAvailable() bool {
	conn, err := net.DialTimeout("tcp", ipcAddr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// sendToDaemon sends one command to a running daemon and streams the reply to
// stdout. forwarded is false when no daemon accepted the connection, so the
// caller can fall back to running the command itself; err is the command's
// failure as reported by the daemon (or a broken connection).
func sendToDaemon(argv []string) (forwarded bool, err error) {
	conn, err := net.DialTimeout("tcp", ipcAddr, 250*time.Millisecond)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", quoteArgs(absSourcePath(argv))); err != nil {
		return true, err
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if status, ok := strings.CutPrefix(line, ipcStatusPrefix); ok {
			if msg, failed := strings.CutPrefix(status, "error "); failed {
				return true, errors.New(msg)
			}
			return true, nil
		}
		fmt.Println(line)
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, errors.New("daemon closed the connection before reporting a result")
}

// absSourcePath makes the script path of a `source`/`run` command absolute,
// since the daemon resolves relative paths against its own working directory.
func absSourcePath(argv []string) []string {
	if len(argv) < 2 || (argv[0] != "source" && argv[0] != "run") {
		return argv
	}
	last := len(argv) - 1
	abs, err := filepath.Abs(argv[last])
	if err != nil {
		return argv
	}
	out := append([]string(nil), argv...)
	out[last] = abs
	return out
}

// quoteArgs rebuilds a command line so the daemon's tokenizer reassembles the
// same tokens: tokens with whitespace or quote characters are wrapped in the
// quote style they don't contain (the tokenizer has no escapes).
func quoteArgs(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a != "" && !strings.ContainsAny(a, " \t\"'"):
			parts[i] = a
		case strings.Contains(a, `"`):
			parts[i] = `'` + a + `'`
		default:
			parts[i] = `"` + a + `"`
		}
	}
	return strings.Join(parts, " ")
}
