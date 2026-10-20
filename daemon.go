package main

// Stage-3 daemon: hold one gateway (and the device connection) resident, react
// to touch gestures, and accept commands over a localhost IPC so one-shot
// `stackchan-cli <cmd>` invocations forward to it (fast, no port conflict)
// instead of each spawning its own gateway.

import (
	"bufio"
	"bytes"
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

// IPC commands and touch reactions run concurrently, each with its own output
// (cmdEnv), so a long command (listen, say) never holds up the others. The
// device still handles the resulting tool calls one at a time, so a command
// sent during a reaction may land between its steps. Reactions never overlap
// each other: reactToTouches runs them one by one.

const ipcAddr = "127.0.0.1:8770"

func cmdDaemon(args []string) error {
	set, verbose := fs("daemon")
	opts := daemonOptions{}
	set.BoolVar(&opts.touch, "touch", true, "react to head touch events")
	set.IntVar(&opts.tapMaxMs, "tap-max", 1000, "touches up to this many ms are a tap; longer ones a stroke")
	set.BoolVar(&opts.avatar, "avatar", true, "send ~/.stackchan/avatar/*.png to the device on every (re)connect")
	set.BoolVar(&opts.voice, "voice", true, "run ~/.stackchan/voice-commands.txt on speech after a screen tap")
	set.StringVar(&opts.reactions, "reactions", filepath.Join(stackchanDir(), "reactions"), "dir of per-gesture reaction scripts (<gesture>.txt)")
	sleepFlag := set.String("sleep", defaultSleepWindow, `daily HH:MM-HH:MM to sleep: touch sensor off, run reactions/sleep.txt ("off" = only on the sleep command)`)
	if err := set.Parse(args); err != nil {
		return err
	}
	var err error
	if opts.sleep, err = parseSleepWindow(*sleepFlag); err != nil {
		return err
	}
	opts.verbose = *verbose
	// Report our own fatal error before flushing, so it reaches the log
	// (main would print it after the timestamp relay is gone).
	flush := timestampStderr()
	err = runDaemon(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	flush()
	switch {
	case errors.Is(err, errGatewayStart), errors.Is(err, errGatewayLost):
		os.Exit(daemonExitRestart)
	case err != nil:
		os.Exit(1)
	}
	return nil
}

// daemonExitRestart is the exit code for "start me again": the gateway did
// not start or exited. start-daemon.vbs restarts the daemon on it (a Go panic
// exits with 2 as well); other failures (another daemon running, a stop by
// Stop-Process) exit with 1 and are not restarted. Task Scheduler's
// restart-on-failure does not help here: it only covers a task that could not
// be launched, not a non-zero exit code.
const daemonExitRestart = 2

// errGatewayLost marks a gateway that exited while the daemon was serving.
var errGatewayLost = errors.New("gateway exited")

// gatewayStartAttempts and gatewayRetryWait: a gateway that once did not
// answer initialize within 20 s started in 3 s right after.
const (
	gatewayStartAttempts = 3
	gatewayRetryWait     = 10 * time.Second
)

func gatewayLogPath() string { return filepath.Join(stackchanDir(), "gateway.log") }

// openGatewayLog opens ~/.stackchan/gateway.log for the gateway's stderr,
// so a failed start leaves its cause. Over 5 MB it moves to gateway.log.1.
func openGatewayLog() (*os.File, error) {
	path := gatewayLogPath()
	if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "=== gateway start %s ===\n", time.Now().Format("2006-01-02 15:04:05.000"))
	}
	return f, err
}

type daemonOptions struct {
	verbose   bool
	touch     bool
	tapMaxMs  int
	avatar    bool
	voice     bool
	reactions string
	sleep     *sleepWindow // nil = sleep only on the sleep command
}

func runDaemon(opts daemonOptions) error {
	reactionsDir = opts.reactions
	seedScripts(reactionsDir, defaultReactions)
	seedScripts(reactionsDir, defaultSleepScripts)
	seedScripts(reactionsDir, defaultTimerScripts)
	seedFile(pcConfigPath(), defaultPCMonitorFile)
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

	// Voice commands: have the gateway POST screen-tap speech to us.
	var voiceLn net.Listener
	var err error
	if opts.voice {
		if user := os.Getenv("STACKCHAN_AUDIO_HOOK_URL"); user != "" {
			fmt.Fprintf(os.Stderr, "daemon: STACKCHAN_AUDIO_HOOK_URL=%s is set; voice commands are off\n", user)
		} else if voiceLn, err = net.Listen("tcp", voiceHookAddr); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: voice commands are off: %v\n", err)
			voiceLn = nil
		} else {
			defer voiceLn.Close()
			seedFile(voiceCommandsPath(), defaultVoiceCommands)
			gatewayEnv = append(gatewayEnv, "STACKCHAN_AUDIO_HOOK_URL=http://"+voiceHookAddr+"/voice")
		}
	}

	// Warm up VOICEVOX in the background so the first reaction can speak. An
	// engine we start lives as long as we do, so it runs on the GPU.
	voicevoxResident = true
	go func() {
		if err := ensureVoicevox(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		}
	}()

	for attempt := 1; ; attempt++ {
		err := serveWithGateway(opts, voiceLn, gatewayEnv)
		if !errors.Is(err, errGatewayStart) || attempt == gatewayStartAttempts {
			return err
		}
		fmt.Fprintf(os.Stderr, "daemon: %v; trying again in %s (see %s)\n", err, gatewayRetryWait, gatewayLogPath())
		time.Sleep(gatewayRetryWait)
		waitPortsFree([]string{":8765", ":8766"}, 15*time.Second)
	}
}

// serveWithGateway starts one gateway and serves IPC on it until either
// ends. IPC is open only for this attempt: between retries clients find no
// daemon and fall back to a gateway of their own instead of waiting.
func serveWithGateway(opts daemonOptions, voiceLn net.Listener, env []string) error {
	ln, err := net.Listen("tcp", ipcAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ipcAddr, err)
	}
	defer ln.Close()
	stderr, closeLog := gatewayStderr(opts.verbose)
	defer closeLog()
	return withGateway(stderr, func(c *mcp.Client) error {
		return serveDaemon(c, ln, voiceLn, opts)
	}, env...)
}

// gatewayLogNoise is the gateway's line for every tool call: the session
// watch alone makes one every 3 s (about 2.5 MB a day).
const gatewayLogNoise = "Processing request of type"

// dropLines writes the lines that do not contain drop. It is only written to
// by the gateway's stderr copier (and flushed after the gateway exited), so
// it needs no lock.
//
// It never fails: an error would stop the copier, nobody would read the
// gateway's stderr any more, and the gateway would hang once the pipe is full.
type dropLines struct {
	w       io.Writer
	drop    string
	partial []byte
}

// dropLinesMax flushes output that never ends a line (a progress bar
// redrawn with '\r'), so it neither grows without bound nor stays unwritten.
const dropLinesMax = 64 << 10

func (d *dropLines) Write(p []byte) (int, error) {
	d.partial = append(d.partial, p...)
	for {
		i := bytes.IndexByte(d.partial, '\n')
		if i < 0 {
			if len(d.partial) > dropLinesMax {
				d.Flush()
			}
			return len(p), nil
		}
		if line := d.partial[:i+1]; !bytes.Contains(line, []byte(d.drop)) {
			_, _ = d.w.Write(line)
		}
		d.partial = d.partial[i+1:]
	}
}

// Flush writes what is left without a final newline, e.g. the last words of
// a gateway that crashed.
func (d *dropLines) Flush() {
	if len(d.partial) > 0 {
		_, _ = d.w.Write(d.partial)
		d.partial = nil
	}
}

// gatewayStderr is where the daemon's gateway writes its log: gateway.log,
// and the daemon's stderr too with --verbose. Call closeLog once the gateway
// has exited.
func gatewayStderr(verbose bool) (w io.Writer, closeLog func()) {
	var ws []io.Writer
	closeLog = func() {}
	if f, err := openGatewayLog(); err == nil {
		d := &dropLines{w: f, drop: gatewayLogNoise}
		ws = append(ws, d)
		closeLog = func() {
			d.Flush()
			_ = f.Close()
		}
	} else {
		fmt.Fprintf(os.Stderr, "daemon: gateway log: %v\n", err)
	}
	if verbose {
		ws = append(ws, os.Stderr)
	}
	return io.MultiWriter(ws...), closeLog
}

// serveDaemon serves IPC on ln and, when voiceLn is set, voice commands on it.
func serveDaemon(c *mcp.Client, ln, voiceLn net.Listener, opts daemonOptions) error {
	fmt.Fprintln(os.Stderr, "daemon: gateway up; waiting for device...")
	fmt.Fprintf(os.Stderr, "daemon: IPC on %s · touch %v (tap ≤ %dms) · reactions %s · Ctrl-C to stop\n", ipcAddr, opts.touch, opts.tapMaxMs, reactionsDir)

	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() { stopOnce.Do(func() { close(stop); ln.Close() }) }

	// A gateway that exits leaves every command failing: end the daemon so
	// start-daemon.vbs starts a new one with a new gateway.
	lost := make(chan struct{})
	go func() {
		select {
		case <-c.Done():
			// Ctrl-C in a console reaches the gateway too, and it may exit
			// before our handler runs: give the stop a moment to arrive.
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			fmt.Fprintln(os.Stderr, "daemon: the gateway exited; stopping (see gateway.log)")
			close(lost)
			shutdown()
		case <-stop:
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\ndaemon: shutting down...")
		shutdown()
	}()

	// IPC is served right away; the device connection is only watched, so
	// commands (e.g. status) never hang while the device is away.
	sl := newSleeper(opts.sleep, c, opts.avatar)
	daemonSleeper = sl // before any command can run
	fmt.Fprintf(os.Stderr, "daemon: sleeps %s and on the sleep command (touch sensor off, reactions sleep.txt / wake.txt)\n", opts.sleep)
	go sl.run(stop)
	go watchSessions(c, sl, stop)
	daemonCPU = &cpuHistory{} // before any command can run
	go watchPC(c, sl, stop)
	daemonTimers = &timerSet{} // before any command can run
	go runTimers(c, daemonTimers, sl, stop)
	if opts.touch {
		// One-slot queue: a touch during a reaction waits and is answered right
		// after it; a newer touch replaces an older waiting one.
		touches := make(chan touchEvent, 1)
		c.OnNotification(func(method string, params json.RawMessage) {
			var ev touchEvent
			if method != "stackchan/event" || json.Unmarshal(params, &ev) != nil || ev.EventType != "touch" {
				return
			}
			ev.at = time.Now()
			queueLatest(touches, ev)
		})
		go reactToTouches(c, touches, time.Duration(opts.tapMaxMs)*time.Millisecond, sl, daemonTimers, stop)
	}
	if voiceLn != nil {
		fmt.Fprintf(os.Stderr, "daemon: voice commands on (rules %s)\n", voiceCommandsPath())
		go serveVoice(voiceLn, newVoiceHook(c), stop)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			select { // lost first: it also closes stop
			case <-lost:
				return errGatewayLost
			default:
			}
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
	errW := &stderrLines{w: conn}
	_, err = runLine(line, cmdEnv{c: c, out: conn, errOut: errW})
	errW.endLine()
	writeIPCStatus(conn, err)
}

// ipcStatusPrefix marks the last line of a daemon reply, which carries the
// command's outcome ("ok" or "error <msg>") so the client can set its exit code.
const ipcStatusPrefix = "\x1estackchan-ipc:"

// ipcStderrPrefix marks a reply line the client writes to its stderr (notes,
// warnings, script errors), as one-shot mode does.
const ipcStderrPrefix = "\x1estackchan-ipc-stderr:"

// stderrLines puts ipcStderrPrefix in front of every line written to w.
type stderrLines struct {
	w   io.Writer
	mid bool // the last write ended inside a line
}

func (s *stderrLines) Write(p []byte) (int, error) {
	buf := make([]byte, 0, len(p)+len(ipcStderrPrefix))
	for rest := p; len(rest) > 0; {
		if !s.mid {
			buf = append(buf, ipcStderrPrefix...)
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			buf = append(buf, rest...)
			s.mid = true
			break
		}
		buf = append(buf, rest[:i+1]...)
		rest = rest[i+1:]
		s.mid = false
	}
	if _, err := s.w.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// endLine finishes an unterminated line, so the status line starts a line.
func (s *stderrLines) endLine() {
	if s.mid {
		_, _ = s.w.Write([]byte("\n"))
		s.mid = false
	}
}

// writeIPCStatus writes the status line that ends a daemon reply.
func writeIPCStatus(w io.Writer, err error) {
	if err != nil {
		// The status must stay on one line.
		msg := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(err.Error())
		fmt.Fprintf(w, "%serror %s\n", ipcStatusPrefix, msg)
		return
	}
	fmt.Fprintf(w, "%sok\n", ipcStatusPrefix)
}

// timestampStderr prefixes every stderr line with the time (to the
// millisecond, to tell where a delay comes from), so the daemon log shows
// when each event happened. It swaps os.Stderr for a pipe, which also
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
				fmt.Fprintf(orig, "%s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), strings.TrimRight(line, "\r\n"))
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
	at         time.Time
}

// queueLatest puts ev in the one-slot queue without blocking the MCP read
// loop, replacing a touch that is still waiting there. The read loop is the
// only producer, so after draining the slot the send always succeeds.
func queueLatest(q chan touchEvent, ev touchEvent) {
	select {
	case old := <-q:
		// Log off the read loop: a stalled stderr relay must not block it.
		go fmt.Fprintf(os.Stderr, "daemon: touch replaced by a newer one while reacting: %s %dms\n", old.Subtype, old.DurationMs)
	default:
	}
	q <- ev
}

// reactToTouches classifies each touch by contact duration (the firmware's
// own label calls anything ≥ 400 ms a stroke, which is too eager) and runs
// the matching reaction. A touch made during a reaction is answered after it,
// unless it came within a second of the touch being answered (one palm
// contact reported as two gestures) or has waited so long the moment passed.
//
// While the pomodoro waits for an answer (timers may be nil), a touch is the
// answer instead. It is taken without waiting for stepMu, which a notice
// (asking for that very answer) may hold for several seconds.
func reactToTouches(c *mcp.Client, touches <-chan touchEvent, tapMax time.Duration, sl *sleeper, timers *timerSet, stop <-chan struct{}) {
	judge := touchJudge{tapMax: tapMax}
	for {
		select {
		case <-stop:
			return
		case ev := <-touches:
			gesture, ignored := judge.judge(ev, time.Now())
			if gesture == "" {
				fmt.Fprintf(os.Stderr, "daemon: touch ignored (%s): %s %dms\n", ignored, ev.Subtype, ev.DurationMs)
				continue
			}
			fmt.Fprintf(os.Stderr, "daemon: touch %s %dms (firmware: %s)\n", gesture, ev.DurationMs, ev.Subtype)
			if timers != nil && answerByTouch(timers, ev.at) {
				continue
			}
			// The sensor is normally off while asleep; this covers a touch
			// made just before sleep, or before the device accepted the change.
			reacted := sl.reacting(func() {
				if waited := time.Since(ev.at); waited > touchMaxWait {
					fmt.Fprintf(os.Stderr, "daemon: touch ignored (waited %s for a notice): %s\n", waited.Round(time.Second), gesture)
					return
				}
				reactToGesture(c, gesture)
				if timers != nil {
					timers.ledsUsed() // reactions end with clear-leds
				}
			})
			if !reacted {
				fmt.Fprintf(os.Stderr, "daemon: touch ignored (asleep): %s %dms\n", ev.Subtype, ev.DurationMs)
			}
		}
	}
}

const (
	touchDuplicateWindow = time.Second      // one palm contact reported as two gestures
	touchMaxWait         = 10 * time.Second // the moment has passed
)

// touchJudge decides how reactToTouches answers each touch.
type touchJudge struct {
	tapMax       time.Duration
	lastAnswered time.Time // arrival time of the last touch we reacted to
}

// judge returns the gesture to react with ("tap" or "stroke"), or "" and why
// the touch is ignored. now is when the touch is taken off the queue.
func (j *touchJudge) judge(ev touchEvent, now time.Time) (gesture, ignored string) {
	if ev.at.Sub(j.lastAnswered) < touchDuplicateWindow {
		return "", "same contact as the previous one"
	}
	if waited := now.Sub(ev.at); waited > touchMaxWait {
		return "", fmt.Sprintf("waited %s during a reaction", waited.Round(time.Second))
	}
	j.lastAnswered = ev.at
	if time.Duration(ev.DurationMs)*time.Millisecond <= j.tapMax {
		return "tap", ""
	}
	return "stroke", ""
}

// watchSessions logs device (re)connections and has sl apply the state to
// each one (avatar set, touch sensor, face, scripts): a loaded avatar set
// lives in device RAM and is gone after a reboot. A new connection is a new
// session_id (gateway ≥ 0.18 reports it, catching reconnects between polls)
// or a disconnected→connected transition.
func watchSessions(c *mcp.Client, sl *sleeper, stop <-chan struct{}) {
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
				// The device comes up awake with its idle face and no avatar
				// set; sl sends the set and face for the state.
				sl.deviceConnected()
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
// edited without rebuilding. A gesture whose file is missing runs these too.
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

// seedScripts writes each scripts[name] to dir/<name>.txt unless it exists.
func seedScripts(dir string, scripts map[string]string) {
	for name, body := range scripts {
		seedFile(filepath.Join(dir, name+".txt"), body)
	}
}

// seedFile writes body to path unless the file exists, so it can be edited.
func seedFile(path, body string) {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, []byte(body), 0o644)
	}
}

// reactToGesture runs the reaction for a gesture: the script file
// reactionsDir/<gesture>.txt if present, else a built-in fallback. Output goes
// to the daemon log (stderr) so it doesn't tangle with forwarded-command output.
func reactToGesture(c *mcp.Client, gesture string) {
	fmt.Fprintf(os.Stderr, "daemon: touch %q -> reacting\n", gesture)
	runNamedScript(c, gesture, defaultReactions, false, nil)
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
	return forwardTo(ipcAddr, argv, os.Stdout, os.Stderr)
}

// forwardTo is sendToDaemon for a daemon on addr, writing the reply to
// stdout and stderr.
func forwardTo(addr string, argv []string, stdout, stderr io.Writer) (forwarded bool, err error) {
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
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
		if msg, ok := strings.CutPrefix(line, ipcStderrPrefix); ok {
			fmt.Fprintln(stderr, msg)
			continue
		}
		fmt.Fprintln(stdout, line)
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
