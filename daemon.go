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
	opts := daemonOptions{}
	set.BoolVar(&opts.touch, "touch", true, "react to head touch events")
	set.IntVar(&opts.tapMaxMs, "tap-max", 1000, "touches up to this many ms are a tap; longer ones a stroke")
	set.BoolVar(&opts.avatar, "avatar", true, "send ~/.stackchan/avatar/*.png to the device on every (re)connect")
	set.StringVar(&opts.reactions, "reactions", filepath.Join(stackchanDir(), "reactions"), "dir of per-gesture reaction scripts (<gesture>.txt)")
	if err := set.Parse(args); err != nil {
		return err
	}
	opts.verbose = *verbose
	// Report our own fatal error before flushing, so it reaches the log
	// (main would print it after the timestamp relay is gone).
	flush := timestampStderr()
	err := runDaemon(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	flush()
	if err != nil {
		os.Exit(1)
	}
	return nil
}

type daemonOptions struct {
	verbose   bool
	touch     bool
	tapMaxMs  int
	avatar    bool
	reactions string
}

func runDaemon(opts daemonOptions) error {
	reactionsDir = opts.reactions
	seedReactions(reactionsDir)
	var gatewayEnv []string
	if opts.touch {
		// Ask the gateway to push touch events as "stackchan/event" notifications.
		if user := os.Getenv("STACKCHAN_NOTIFY_CONFIG"); user != "" {
			fmt.Fprintf(os.Stderr, "daemon: using STACKCHAN_NOTIFY_CONFIG=%s; touch reactions need legacy_event.enabled: true in it\n", user)
		} else {
			path, err := writeNotifyConfig()
			if err != nil {
				return fmt.Errorf("notify config: %w", err)
			}
			gatewayEnv = append(gatewayEnv, "STACKCHAN_NOTIFY_CONFIG="+path)
		}
	}

	if daemonAvailable() {
		return fmt.Errorf("a daemon is already running on %s", ipcAddr)
	}
	// A gateway that was just stopped may still hold its ports for a moment;
	// starting ours then fails with "gateway closed stdout". Wait before
	// opening IPC, so clients fall back instead of queuing on a daemon that
	// is not serving yet.
	waitPortsFree([]string{":8765", ":8766"}, 15*time.Second)
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

	return withClient(opts.verbose, func(c *mcp.Client) error {
		return serveDaemon(c, ln, opts)
	}, gatewayEnv...)
}

func serveDaemon(c *mcp.Client, ln net.Listener, opts daemonOptions) error {
	fmt.Fprintln(os.Stderr, "daemon: gateway up; waiting for device...")
	fmt.Fprintf(os.Stderr, "daemon: IPC on %s · touch %v (tap ≤ %dms) · reactions %s · Ctrl-C to stop\n", ipcAddr, opts.touch, opts.tapMaxMs, reactionsDir)

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

	// IPC is served right away; the device connection is only watched, so
	// commands (e.g. status) never hang while the device is away.
	go watchSessions(c, opts.avatar, stop)
	if opts.touch {
		// Unbuffered: a touch is handed over only while the reactor is idle,
		// so touches during a reaction are dropped instead of queued.
		touches := make(chan touchEvent)
		c.OnNotification(func(method string, params json.RawMessage) {
			var ev touchEvent
			if method != "stackchan/event" || json.Unmarshal(params, &ev) != nil || ev.EventType != "touch" {
				return
			}
			select { // never block the read loop
			case touches <- ev:
			default:
			}
		})
		go reactToTouches(c, touches, time.Duration(opts.tapMaxMs)*time.Millisecond, stop)
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

// touchEvent is the params of the gateway's "stackchan/event" notification.
// The firmware detects touches locally at 100 Hz and reports each finished
// gesture with its contact duration, so nothing is lost to slow Wi-Fi.
type touchEvent struct {
	EventType  string `json:"event_type"`
	Subtype    string `json:"subtype"`
	DurationMs int    `json:"duration_ms"`
}

// reactToTouches classifies each touch by contact duration (the firmware's
// own label calls anything ≥ 400 ms a stroke, which is too eager) and runs
// the matching reaction. Touches arriving during a reaction are dropped by
// the notification handler; a short cooldown after each reaction absorbs a
// palm contact the firmware reports as two gestures.
func reactToTouches(c *mcp.Client, touches <-chan touchEvent, tapMax time.Duration, stop <-chan struct{}) {
	const cooldown = time.Second
	var lastEnd time.Time
	for {
		select {
		case <-stop:
			return
		case ev := <-touches:
			if time.Since(lastEnd) < cooldown {
				continue
			}
			gesture := "stroke"
			if time.Duration(ev.DurationMs)*time.Millisecond <= tapMax {
				gesture = "tap"
			}
			fmt.Fprintf(os.Stderr, "daemon: touch %s %dms (firmware: %s)\n", gesture, ev.DurationMs, ev.Subtype)
			reactToGesture(c, gesture)
			lastEnd = time.Now()
		}
	}
}

// watchSessions logs device (re)connections and, when enabled, re-sends the
// custom avatar set after each one: a loaded set lives in device RAM and is
// gone after a reboot. A new connection is a new session_id (gateway ≥ 0.18
// reports it, catching reconnects between polls) or a disconnected→connected
// transition.
func watchSessions(c *mcp.Client, avatar bool, stop <-chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	lastSession, wasConnected := "", false
	for {
		res, err := c.CallTool("get_status", map[string]any{})
		if err == nil {
			connected, session := parseStatus(res.Text())
			isNew := connected && (!wasConnected || session != lastSession)
			wasConnected, lastSession = connected, session
			if isNew {
				fmt.Fprintf(os.Stderr, "daemon: device connected (session %s)\n", session)
				if avatar {
					// Not under execMu: commands keep working during the upload.
					// The firmware sometimes shows the new face but never sends
					// its "loaded" reply, so a timeout here is not conclusive.
					if text, err := loadAvatarQuiet(c); err != nil {
						fmt.Fprintf(os.Stderr, "daemon: avatar load: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "daemon: avatar loaded %s\n", text)
					}
				}
			}
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// waitPortsFree waits until every addr can be listened on, or the timeout
// passes (then the gateway start reports the real error).
func waitPortsFree(addrs []string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, a := range addrs {
		for {
			l, err := net.Listen("tcp", a)
			if err == nil {
				_ = l.Close()
				break
			}
			if time.Now().After(deadline) {
				return
			}
			fmt.Fprintf(os.Stderr, "daemon: waiting for port %s to be released...\n", a)
			time.Sleep(time.Second)
		}
	}
}

// writeNotifyConfig (re)writes the daemon-owned ~/.stackchan/notify.yml that
// enables the gateway's legacy_event channel ("stackchan/event"
// notifications). Rewritten on every start so an edit cannot silently
// disable touch reactions.
func writeNotifyConfig() (string, error) {
	path := filepath.Join(stackchanDir(), "notify.yml")
	if err := os.MkdirAll(stackchanDir(), 0o755); err != nil {
		return "", err
	}
	body := "# Owned by stackchan-cli daemon (rewritten on start): push touch events as MCP notifications.\nlegacy_event:\n  enabled: true\n"
	return path, os.WriteFile(path, []byte(body), 0o644)
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
