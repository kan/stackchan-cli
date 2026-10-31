// Command stackchan-cli controls an M5 StackChan robot through the
// stackchan-mcp gateway over stdio MCP (JSON-RPC 2.0): initialize -> tools/call.
//
// Two modes:
//   - one-shot: each command boots a fresh gateway, runs one tool, and exits.
//     Device commands first wait for the StackChan to (re)connect.
//   - repl: boot the gateway once and keep it (and the device connection)
//     resident, so commands run instantly without per-command reconnects.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"stackchan-cli/internal/mcp"
)

// cmdEnv is what one command runs against. Each command gets its own, so the
// daemon can run several at once (an IPC command during a touch reaction)
// without their output mixing.
type cmdEnv struct {
	c      *mcp.Client // nil in one-shot mode: the command spawns a gateway
	out    io.Writer   // command results
	errOut io.Writer   // notes, warnings and per-line script errors
	// inVoice is set while voice rules run, so a rule cannot call voice
	// again (it could recurse until the daemon crashes).
	inVoice bool
	// deviceErrors makes a tool result carrying "error" fail the command
	// (set for sleep.txt / wake.txt, which are retried on failure).
	deviceErrors bool
}

// stdEnv writes to the terminal (one-shot and REPL).
func stdEnv(c *mcp.Client) cmdEnv { return cmdEnv{c: c, out: os.Stdout, errOut: os.Stderr} }

// logEnv writes to the daemon log (what the daemon does on its own: touch
// reactions, sleep, voice commands, notices).
func logEnv(c *mcp.Client) cmdEnv { return cmdEnv{c: c, out: os.Stderr, errOut: os.Stderr} }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	killChildrenOnExit()

	switch cmd {
	case "-h", "--help", "help":
		usage()
		return
	case "repl":
		if err := cmdRepl(args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "daemon":
		if err := cmdDaemon(args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// If a daemon is running, forward to it (fast; reuses its resident gateway,
	// avoiding a second gateway / port conflict). Otherwise spawn a one-shot
	// gateway per command.
	forwarded, err := sendToDaemon(os.Args[1:])
	if !forwarded {
		if cmd == "source" || cmd == "run" {
			err = cmdSource(args)
		} else {
			err = dispatch(cmd, args, stdEnv(nil))
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes a command name to its handler. e.c is nil in one-shot mode
// (handlers spawn a fresh gateway) or a live client in REPL/daemon mode
// (reused, so the gateway and device connection persist between commands).
func dispatch(cmd string, args []string, e cmdEnv) error {
	switch cmd {
	case "status":
		return cmdStatus(args, e)
	case "tools":
		return cmdTools(args, e)
	case "wait":
		return cmdWait(args, e)
	case "move-head":
		return cmdMoveHead(args, e)
	case "avatar":
		return cmdAvatar(args, e)
	case "avatar-load":
		return cmdAvatarLoad(args, e)
	case "mouth":
		return cmdMouth(args, e)
	case "blink":
		return cmdBlink(args, e)
	case "led":
		return cmdLED(args, e)
	case "all-leds":
		return cmdAllLEDs(args, e)
	case "clear-leds":
		return cmdClearLEDs(args, e)
	case "volume":
		return cmdVolume(args, e)
	case "brightness":
		return cmdBrightness(args, e)
	case "say":
		return cmdSay(args, e)
	case "stop":
		return cmdStop(args, e)
	case "listen":
		return cmdListen(args, e)
	case "voice":
		return cmdVoice(args, e)
	case "pc":
		return cmdPC(args, e)
	case "ask":
		return cmdAsk(args, e)
	case "timer":
		return cmdTimer(args, e)
	case "pomodoro":
		return cmdPomodoro(args, e)
	case "photo":
		return cmdPhoto(args, e)
	case "call":
		return cmdCall(args, e)
	case "sleep":
		return cmdSleep(args, e)
	case "wakeup":
		return cmdWakeup(e)
	default:
		return fmt.Errorf("unknown command: %s (try 'help')", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stackchan-cli — control an M5 StackChan via the stackchan-mcp gateway

Usage:
  stackchan-cli <command> [flags]
  stackchan-cli repl                  Interactive session (gateway stays resident; fast)
  stackchan-cli source <file>         Run a script file (sleep/repeat/# comments) in one session
  stackchan-cli daemon                Resident gateway: touch reactions + fast command forwarding

While a daemon is running, commands, repl and source are forwarded to it.

Commands:
  status                 Show gateway/device connection status (no device needed)
  tools                  List the tools the gateway exposes (no device needed)
  wait [--timeout N]     Keep the gateway up and wait for the device to connect
  move-head --yaw N --pitch N
  avatar <face>          idle|happy|thinking|sad|surprised|embarrassed|off
  avatar-load [--set NAME]  upload ~/.stackchan/avatar/*.png (or avatar-NAME/, e.g. sleep)
                         as the face set (lost on device reboot;
                         the daemon re-sends it automatically)
  mouth <shape>          closed|half|open|e|u
  blink <on|off>
  led --index N --r N --g N --b N
  all-leds --r N --g N --b N
  clear-leds
  volume <0-100>
  brightness <0-100>
  say [--speaker N] [--face F] [--while-face F] <text>
                         speak via VOICEVOX (engine auto-started if needed);
                         --face after speaking, --while-face while speaking
  stop                   stop what is being said, at the end of the sentence
                         (a head touch while speaking does the same; needs the daemon)
  listen [--sec N] [--lang ja] [--model M] [--motion none|face-only|look-up]
                         record from the device mic and print the transcription
                         (needs the gateway's stt-faster-whisper extra)
  voice [text]           run ~/.stackchan/voice-commands.txt on text as if it were heard;
                         without text, list the rules
                         (the daemon runs them on speech: tap the screen, speak, tap again)
  pc                     show this PC's CPU, memory and disk space, and what say's {pc}
                         would say (settings: ~/.stackchan/pc-monitor.txt)
  ask --open             yes to "パソコンでページを開く？": open the page (needs the daemon)
  ask [--quiet] <question>  ask Claude (claude -p, web search allowed) and speak the
                         answer (--quiet: print only); settings ~/.stackchan/ask.txt,
                         persona ~/.stackchan/ask/system.txt; a URL in the answer is
                         not read aloud: the daemon asks whether to open it
                         (yes: a head touch, a spoken yes, or ask --open)
  timer <dur> [--quiet]  start a timer (3m, 1h30m, 90s, 3分); speaks when it is up (needs the daemon)
  timer [--say]          list the timers and the pomodoro (--say: speak it)
  timer cancel           cancel the timers (not the pomodoro)
  pomodoro start [--work 25m --break 5m --long 15m --every 4]
                         work, then a break (every 4th long), round and round; LEDs show
                         the time left; the end of work waits for a head touch or pomodoro ok
  pomodoro stop | ok     stop it | answer the end of work (start the break)
  pomodoro               show what it is doing
  photo [--question "..."] [--open]   capture a photo; saved to ~/.stackchan/captures
  call <tool> [--json '{...}']   Invoke any tool with raw JSON arguments
  sleep                  sleep until wakeup or the end of the daemon's --sleep window
                         (07:00 by default; with --sleep off, until wakeup): touch
                         sensor off, say goodnight.txt, run ~/.stackchan/reactions/sleep.txt
                         (needs the daemon)
  wakeup                 wake up: touch sensor on, run reactions/wake.txt, say goodmorning.txt
                         (needs the daemon)
  sleep <ms>             pause (for scripts)

Environment:
  STACKCHAN_MCP_EXE   path to the gateway executable (default: PATH / ~/.local/bin)
  STACKCHAN_TOKEN     bearer token shared with the firmware (empty = no auth)
  VISION_HOST         this host's LAN IP for 'photo' (auto-detected if unset)
  STACKCHAN_VOICEVOX_URL        VOICEVOX engine URL (default http://127.0.0.1:50021)
  STACKCHAN_VOICEVOX_ENGINE     path to vv-engine\run.exe (default: standard install dirs)
  STACKCHAN_VOICEVOX_AUTOSTART  0 = never start the engine automatically
  STACKCHAN_FASTER_WHISPER_MODEL  speech recognition model for listen (default small)

Per-command flags:
  --verbose           forward the gateway's stderr logs (one-shot mode)

Without a daemon, every command restarts the gateway, so device commands wait
up to 90s for the StackChan to reconnect. Use 'daemon' or 'repl' for snappy control.
`)
}

// ---- shared plumbing -------------------------------------------------------

// withClient boots a gateway, initializes the session, runs fn, then shuts down.
// extraEnv is appended to the gateway's environment.
func withClient(verbose bool, fn func(*mcp.Client) error, extraEnv ...string) error {
	var stderr io.Writer
	if verbose {
		stderr = os.Stderr
	}
	return withGateway(stderr, fn, extraEnv...)
}

// errGatewayStart marks a gateway that started but did not answer
// initialize: worth trying again (once it took over 20 s, and 3 s right
// after). A gateway that cannot be launched at all (no executable) is not.
var errGatewayStart = errors.New("gateway did not start")

// withGateway is withClient with the gateway's stderr going to stderr (nil
// discards it).
func withGateway(stderr io.Writer, fn func(*mcp.Client) error, extraEnv ...string) error {
	exe := gatewayPath()
	env := append(gatewayEnv(), extraEnv...)

	c, err := mcp.Start(exe, gatewayArgs(), env, stderr)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		return fmt.Errorf("%w: %w", errGatewayStart, err) // err reads "initialize: ..."
	}
	return fn(c)
}

// gatewayArgs runs the gateway without mDNS unless STACKCHAN_MDNS=1. Its
// advertisement adds about 1.4 s to every start, can stall on a host with
// many interfaces (per the gateway's own comments), and the device here
// connects by its configured URL anyway.
func gatewayArgs() []string {
	if os.Getenv("STACKCHAN_MDNS") == "1" {
		return []string{"serve"}
	}
	return []string{"serve", "--no-mdns"}
}

// withClientOrReuse runs fn against an existing client (REPL/daemon mode, c != nil) or
// against a freshly spawned one-shot gateway (c == nil).
func withClientOrReuse(verbose bool, c *mcp.Client, fn func(*mcp.Client) error) error {
	if c != nil {
		return fn(c)
	}
	return withClient(verbose, fn)
}

// withDevice is withClientOrReuse for commands that need the StackChan: a
// freshly started one-shot gateway first waits for the device to reconnect
// (a resident client is already connected).
func withDevice(verbose bool, c *mcp.Client, fn func(*mcp.Client) error) error {
	oneShot := c == nil
	return withClientOrReuse(verbose, c, func(c *mcp.Client) error {
		if oneShot {
			if err := waitForDevice(c); err != nil {
				return err
			}
		}
		return fn(c)
	})
}

func gatewayPath() string {
	if p := os.Getenv("STACKCHAN_MCP_EXE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("stackchan-mcp"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "stackchan-mcp.exe")
}

// defaultWhisperModel is the speech recognition model for listen and voice
// commands: the gateway's default "base" misheard "スタックチャン" as
// "スタッフちゃん"; "small" got it right (~4s on CPU).
const defaultWhisperModel = "small"

func gatewayEnv() []string {
	env := os.Environ()
	if os.Getenv("STACKCHAN_TOKEN") == "" {
		fmt.Fprintln(os.Stderr, "note: STACKCHAN_TOKEN not set; gateway accepts any client (no auth). This matches a firmware with an empty websocket.token.")
	}
	// take_photo needs the device to POST the JPEG back to this host. If the
	// user hasn't pinned VISION_HOST/VISION_URL, auto-detect this machine's LAN
	// IP so photo works out of the box (override by setting VISION_HOST).
	if os.Getenv("VISION_HOST") == "" && os.Getenv("VISION_URL") == "" {
		if ip := localLANIP(); ip != "" {
			fmt.Fprintf(os.Stderr, "note: VISION_HOST not set; using auto-detected LAN IP %s (set VISION_HOST to override).\n", ip)
			env = append(env, "VISION_HOST="+ip)
		}
	}
	// Personal default voice: 春日部つむぎ (VOICEVOX speaker 8). Override by
	// setting STACKCHAN_VOICEVOX_DEFAULT_SPEAKER or passing `say --speaker N`.
	if os.Getenv("STACKCHAN_VOICEVOX_DEFAULT_SPEAKER") == "" {
		env = append(env, "STACKCHAN_VOICEVOX_DEFAULT_SPEAKER="+defaultVoicevoxSpeaker)
	}
	// The same speech recognition settings as voice commands (see stt.go).
	return append(env, whisperEnv(sttPython())...)
}

// localLANIP returns this host's primary LAN IPv4 by inspecting which local
// address the OS would use to reach an external host. No packets are sent (UDP
// "connect" only resolves the route), so it works offline as long as a default
// route exists. Returns "" if it can't be determined.
func localLANIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// callAndPrint runs one tool and prints its text result. In one-shot mode
// (e.c == nil) it spawns a gateway and, when waitDevice is set, blocks until
// the StackChan reconnects. In REPL/daemon mode (e.c != nil) it reuses the resident
// client and skips the wait, since the device stays connected between commands.
func callAndPrint(verbose, waitDevice bool, tool string, args map[string]any, e cmdEnv) error {
	run := func(c *mcp.Client) error {
		res, err := callTool(c, tool, args, e.out)
		if err == nil && e.deviceErrors {
			if msg := resultError(res.Text()); msg != "" {
				return fmt.Errorf("%s: %s", tool, msg)
			}
		}
		return err
	}
	if waitDevice {
		return withDevice(verbose, e.c, run)
	}
	return withClientOrReuse(verbose, e.c, run)
}

// waitForDevice blocks a one-shot command until the freshly started gateway
// sees the StackChan reconnect.
func waitForDevice(c *mcp.Client) error {
	fmt.Fprintln(os.Stderr, "waiting for device to connect...")
	return waitConnected(c, 90*time.Second)
}

// longToolTimeouts lists tools that block longer than the client default.
var longToolTimeouts = map[string]time.Duration{
	"say":             5 * time.Minute, // blocks until playback ends
	"take_photo":      60 * time.Second,
	"listen":          5 * time.Minute, // capture ≤ 30s + transcription (first call downloads the model)
	"load_avatar_set": (avatarLoadTimeout + 10) * time.Second,
}

// audioTools use the device's speaker or mic. They run one at a time even in
// the daemon, where commands otherwise run concurrently: a reaction's say
// during listen would be recorded, and two says would mix their audio streams.
var (
	audioTools = map[string]bool{"say": true, "listen": true}
	audioMu    sync.Mutex
)

// callTool runs one tool and returns an error (so the exit code is non-zero)
// when the tool failed. With w set, the result text is printed there; with w
// nil (the caller reads the result itself), a failure's text goes into the
// error instead.
func callTool(c *mcp.Client, tool string, args map[string]any, w io.Writer) (mcp.ToolResult, error) {
	if audioTools[tool] {
		audioMu.Lock()
		defer audioMu.Unlock()
	}
	return callToolHeld(c, tool, args, w)
}

// callToolHeld is callTool for a caller that holds audioMu itself (say,
// which keeps it across its sentences).
func callToolHeld(c *mcp.Client, tool string, args map[string]any, w io.Writer) (mcp.ToolResult, error) {
	res, err := c.CallToolTimeout(tool, args, longToolTimeouts[tool])
	if err != nil {
		return res, err
	}
	if w != nil {
		fmt.Fprintln(w, res.Text())
	}
	if res.IsError {
		if w != nil {
			return res, fmt.Errorf("tool %s failed", tool) // details already printed
		}
		return res, fmt.Errorf("tool %s failed: %s", tool, res.Text())
	}
	return res, nil
}

// waitConnected polls get_status until the device reports connected:true.
func waitConnected(c *mcp.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		res, err := c.CallTool("get_status", map[string]any{})
		if err != nil {
			return err
		}
		if isConnected(res.Text()) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device did not connect within %s", timeout)
		}
		time.Sleep(1 * time.Second)
	}
}

func isConnected(statusText string) bool {
	connected, _ := parseStatus(statusText)
	return connected
}

// parseStatus reads the gateway's get_status result.
func parseStatus(statusText string) (connected bool, sessionID string) {
	var s struct {
		Connected bool   `json:"connected"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(statusText), &s) != nil {
		return false, ""
	}
	return s.Connected, s.SessionID
}

// fs builds a FlagSet with a shared --verbose flag. ContinueOnError keeps a bad
// flag from calling os.Exit (which would kill the REPL).
func fs(name string) (*flag.FlagSet, *bool) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	verbose := set.Bool("verbose", false, "forward gateway stderr logs")
	return set, verbose
}

// ---- commands --------------------------------------------------------------

func cmdStatus(args []string, e cmdEnv) error {
	set, verbose := fs("status")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, false, "get_status", map[string]any{}, e)
}

func cmdTools(args []string, e cmdEnv) error {
	set, verbose := fs("tools")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withClientOrReuse(*verbose, e.c, func(c *mcp.Client) error {
		tools, err := c.ListTools()
		if err != nil {
			return err
		}
		for _, t := range tools {
			var schema struct {
				Required []string `json:"required"`
			}
			_ = json.Unmarshal(t.InputSchema, &schema)
			fmt.Fprintf(e.out, "%-24s %v\n", t.Name, schema.Required)
		}
		fmt.Fprintf(e.errOut, "\n%d tools\n", len(tools))
		return nil
	})
}

func cmdWait(args []string, e cmdEnv) error {
	set, verbose := fs("wait")
	timeout := set.Int("timeout", 60, "seconds to wait for the device to connect")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withClientOrReuse(*verbose, e.c, func(c *mcp.Client) error {
		deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
		for {
			res, err := c.CallTool("get_status", map[string]any{})
			if err != nil {
				return err
			}
			txt := strings.Join(strings.Fields(res.Text()), " ")
			fmt.Fprintf(e.out, "[%s] %s\n", time.Now().Format("15:04:05"), txt)
			if isConnected(txt) {
				fmt.Fprintln(e.out, "device connected ✓")
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("device did not connect within %ds (check Wi-Fi, mDNS, and Windows Firewall on port 8765)", *timeout)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func cmdMoveHead(args []string, e cmdEnv) error {
	set, verbose := fs("move-head")
	yaw := set.Int("yaw", 0, "horizontal angle, -90..90")
	pitch := set.Int("pitch", 45, "vertical angle, 5..85")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "move_head", map[string]any{"yaw": *yaw, "pitch": *pitch}, e)
}

func cmdAvatar(args []string, e cmdEnv) error {
	set, verbose := fs("avatar")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("avatar requires a face (idle|happy|thinking|sad|surprised|embarrassed|off)")
	}
	return callAndPrint(*verbose, true, "set_avatar", map[string]any{"face": set.Arg(0)}, e)
}

func cmdMouth(args []string, e cmdEnv) error {
	set, verbose := fs("mouth")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("mouth requires a shape (closed|half|open|e|u)")
	}
	return callAndPrint(*verbose, true, "set_mouth", map[string]any{"mouth": set.Arg(0)}, e)
}

func cmdBlink(args []string, e cmdEnv) error {
	set, verbose := fs("blink")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("blink requires on|off")
	}
	enabled, err := parseOnOff(set.Arg(0))
	if err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_blink", map[string]any{"enabled": enabled}, e)
}

func cmdVolume(args []string, e cmdEnv) error {
	set, verbose := fs("volume")
	if err := set.Parse(args); err != nil {
		return err
	}
	n, err := intArg(set, "volume", 0, 100)
	if err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_volume", map[string]any{"volume": n}, e)
}

func cmdBrightness(args []string, e cmdEnv) error {
	set, verbose := fs("brightness")
	if err := set.Parse(args); err != nil {
		return err
	}
	n, err := intArg(set, "brightness", 0, 100)
	if err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_brightness", map[string]any{"brightness": n}, e)
}

func cmdClearLEDs(args []string, e cmdEnv) error {
	set, verbose := fs("clear-leds")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "clear_leds", map[string]any{}, e)
}

// parseOnOff accepts on/off/true/false/1/0/yes/no.
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("expected on|off, got %q", s)
}

// intArg reads the first positional as an int and range-checks it.
func intArg(set *flag.FlagSet, name string, lo, hi int) (int, error) {
	if set.NArg() < 1 {
		return 0, fmt.Errorf("%s requires a number (%d..%d)", name, lo, hi)
	}
	n, err := strconv.Atoi(set.Arg(0))
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, set.Arg(0))
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s must be %d..%d", name, lo, hi)
	}
	return n, nil
}

func cmdLED(args []string, e cmdEnv) error {
	set, verbose := fs("led")
	index := set.Int("index", 0, "LED index 0..11")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_led", map[string]any{"index": *index, "r": *r, "g": *g, "b": *b}, e)
}

func cmdAllLEDs(args []string, e cmdEnv) error {
	set, verbose := fs("all-leds")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_all_leds", map[string]any{"r": *r, "g": *g, "b": *b}, e)
}

// restFace is the face StackChan rests in: after speaking, after a voice
// command is transcribed, and when the device connects (its own is idle).
const restFace = "embarrassed"

// expandSayVars fills in the placeholders say text can use, so a rule like
// `say "{date}、{weekday}、{time}です"` tells the time. Unknown ones stay as is.
func expandSayVars(text string, now time.Time) string {
	if !strings.Contains(text, "{") {
		return text
	}
	weekdays := [...]string{"日", "月", "火", "水", "木", "金", "土"}
	return strings.NewReplacer(
		"{date}", fmt.Sprintf("%d年%d月%d日", now.Year(), now.Month(), now.Day()),
		"{weekday}", weekdays[now.Weekday()]+"曜日",
		"{time}", fmt.Sprintf("%d時%d分", now.Hour(), now.Minute()),
	).Replace(text)
}

func cmdSay(args []string, e cmdEnv) error {
	set, verbose := fs("say")
	speaker := set.Int("speaker", 0, "VOICEVOX speaker id (0 = gateway default; e.g. 3=ずんだもん, 8=春日部つむぎ)")
	face := set.String("face", restFace, "avatar face to set after speaking (\"\" = leave unchanged)")
	whileFace := set.String("while-face", "", "avatar face to show while speaking (\"\" = the one {pc} picks, else leave unchanged)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("say requires text (put flags first, e.g. say --speaker 8 \"...\")")
	}
	text := strings.Join(set.Args(), " ")
	sayArgs := map[string]any{}
	if *speaker > 0 {
		sayArgs["speaker_id"] = *speaker
	}
	// Start the engine, and fetch the weather if the text asks for it, while a
	// one-shot gateway waits for the device.
	engineReady := make(chan error, 1)
	go func() { engineReady <- ensureVoicevox() }()
	// Its notes are written by this command's goroutine, not alongside it.
	// The PC values are read here too: without the daemon's record, the CPU
	// is measured over a second.
	type filled struct{ text, face, notes string }
	expanded := make(chan filled, 1)
	go func() {
		var notes strings.Builder
		t, err := expandWeather(text, time.Now(), &notes)
		if err != nil {
			fmt.Fprintf(&notes, "warning: %v\n", err)
		}
		t, face := expandPC(t, &notes)
		expanded <- filled{t, face, notes.String()}
	}()
	return withDevice(*verbose, e.c, func(c *mcp.Client) error {
		if err := <-engineReady; err != nil {
			fmt.Fprintf(e.errOut, "warning: %v\n", err)
		}
		w := <-expanded
		fmt.Fprint(e.errOut, w.notes)
		during := *whileFace
		if during == "" {
			during = w.face // {pc} shows how the PC is doing
		}
		setFace(c, during, e.errOut)
		// Filled in now, not before a one-shot gateway waited for the device.
		full := expandSayVars(w.text, time.Now())
		sp := daemonSpeech
		sentences := splitSentences(full)
		if sp == nil || len(sentences) == 0 {
			sentences = []string{full} // nothing can stop it: one call
		}
		say := func(text string) error {
			sayArgs["text"] = text
			res, err := callToolHeld(c, "say", sayArgs, e.out)
			if err != nil {
				return err
			}
			// The gateway reports a device timeout as a result. The rest
			// would time out too, holding audioMu for each sentence.
			if msg := resultError(res.Text()); msg != "" {
				if e.deviceErrors {
					return fmt.Errorf("say: %s", msg)
				}
				return errSayRest
			}
			return nil
		}
		if err := speakSentences(sp, sentences, say, e.out); err != nil {
			if during != "" {
				setFace(c, *face, e.errOut) // do not leave the face for speaking on
			}
			return err
		}
		// say() blocks until playback finishes, so the mouth has stopped by now;
		// settle on a resting face.
		setFace(c, *face, e.errOut)
		return nil
	})
}

// errSayRest from a sentence's say skips the rest without failing the say
// (the result, printed already, carries the error).
var errSayRest = errors.New("skip the rest of the say")

// speakSentences says each sentence with its own say call. audioMu is held
// across them, so no other say or listen slips in between sentences. With
// sp (the daemon), the say counts as speaking from here on, and a stop
// ends it before the next sentence.
func speakSentences(sp *speech, sentences []string, say func(text string) error, out io.Writer) error {
	audioMu.Lock()
	defer audioMu.Unlock()
	var h speaking
	if sp != nil {
		h = sp.begin()
		defer sp.end(h)
	}
	for i, s := range sentences {
		if sp != nil && sp.stopped(h) {
			fmt.Fprintf(out, "say: stopped (%d of %d sentences said)\n", i, len(sentences))
			return nil
		}
		if err := say(s); err != nil {
			if errors.Is(err, errSayRest) {
				return nil
			}
			return err
		}
	}
	return nil
}

// setFace sets the avatar face, warning to w when it fails. "" does nothing.
func setFace(c *mcp.Client, face string, w io.Writer) {
	if face == "" {
		return
	}
	if _, err := callTool(c, "set_avatar", map[string]any{"face": face}, nil); err != nil {
		fmt.Fprintf(w, "warning: set_avatar %s failed: %v\n", face, err)
	}
}

func cmdListen(args []string, e cmdEnv) error {
	set, verbose := fs("listen")
	sec := set.Float64("sec", 5, "seconds to record, 0.1..30")
	lang := set.String("lang", "ja", "language code for transcription")
	// Default none: the face change waits on the device, which sometimes times
	// out and fails the whole listen.
	motion := set.String("motion", "none", "while listening: none | face-only (thinking face) | look-up")
	model := set.String("model", "", "STT model, e.g. base | small | medium (default: the gateway's STACKCHAN_FASTER_WHISPER_MODEL, small unless set)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if !(*sec >= 0.1 && *sec <= 30) { // also rejects NaN
		return fmt.Errorf("--sec must be 0.1..30")
	}
	listenArgs := map[string]any{
		"duration_ms": int(math.Round(*sec * 1000)),
		"language":    *lang,
		"motion":      *motion,
	}
	if *model != "" {
		listenArgs["model"] = *model
	}
	return withDevice(*verbose, e.c, func(c *mcp.Client) error {
		res, err := callTool(c, "listen", listenArgs, nil)
		if err != nil {
			return err
		}
		var r struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal([]byte(res.Text()), &r) != nil || r.Text == nil {
			fmt.Fprintln(e.out, res.Text())
			return fmt.Errorf("listen: no transcription in the result")
		}
		fmt.Fprintf(e.out, "heard: %s\n", *r.Text)
		return nil
	})
}

func cmdPhoto(args []string, e cmdEnv) error {
	set, verbose := fs("photo")
	question := set.String("question", "What do you see?", "question to ask about the photo")
	open := set.Bool("open", false, "open the saved image in the default viewer")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withDevice(*verbose, e.c, func(c *mcp.Client) error {
		res, err := callTool(c, "take_photo", map[string]any{"question": *question}, e.out)
		if err != nil {
			return err
		}
		path := savedImagePath(res.Text())
		if path == "" {
			fmt.Fprintf(e.errOut, "(image not located; check %s)\n", capturesDir())
			return nil
		}
		fmt.Fprintf(e.out, "photo saved: %s\n", path)
		if *open {
			openInViewer(path)
		}
		return nil
	})
}

// stackchanDir is ~/.stackchan, home of captures, reactions, history and logs.
func stackchanDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stackchan")
}

// expandHome turns a leading ~ into the home directory (for paths in the
// settings files).
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

func capturesDir() string {
	return filepath.Join(stackchanDir(), "captures")
}

// savedImagePath returns the capture file path: prefer image_path from the tool
// result JSON, else fall back to the newest *.jpg in the captures dir.
func savedImagePath(resultText string) string {
	var r struct {
		ImagePath string `json:"image_path"`
	}
	if json.Unmarshal([]byte(resultText), &r) == nil && r.ImagePath != "" {
		return r.ImagePath
	}
	entries, err := os.ReadDir(capturesDir())
	if err != nil {
		return ""
	}
	var newest string
	var newestMod int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if m := info.ModTime().UnixNano(); m > newestMod {
			newestMod, newest = m, filepath.Join(capturesDir(), e.Name())
		}
	}
	return newest
}

// openInViewer opens a file with the OS default handler (Windows shell). The
// viewer is started outside our kill-on-exit job so it outlives the CLI.
func openInViewer(path string) {
	cmd := exec.Command("cmd", "/c", "start", "", path)
	if err := startDetached(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "could not open %s: %v\n", path, err)
	}
}

func cmdCall(args []string, e cmdEnv) error {
	tool, toolArgs, verbose, err := parseCallArgs(args)
	if err != nil {
		return err
	}
	return callAndPrint(verbose, true, tool, toolArgs, e)
}

// parseInterspersed parses flags before and after the positional arguments
// (`timer 3m --quiet`, `call <tool> --json ...`) and returns the positional
// ones: flag parsing stops at the first non-flag, so it parses again after
// each one. After "--" everything is positional.
func parseInterspersed(set *flag.FlagSet, args []string) (pos []string, err error) {
	for {
		if err := set.Parse(args); err != nil {
			return nil, err
		}
		rest := set.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseCallArgs reads `call` arguments: a tool name and --json, in either order.
func parseCallArgs(args []string) (tool string, toolArgs map[string]any, verbose bool, err error) {
	set, verboseFlag := fs("call")
	jsonArgs := set.String("json", "{}", "tool arguments as a JSON object")
	// Flags may come before and after the tool name (call <tool> --json ...).
	pos, err := parseInterspersed(set, args)
	if err != nil {
		return "", nil, false, err
	}
	if len(pos) < 1 {
		return "", nil, false, fmt.Errorf("call requires a tool name")
	}
	if len(pos) > 1 {
		return "", nil, false, fmt.Errorf("call: unexpected arguments %q (put tool arguments in --json)", pos[1:])
	}
	tool = pos[0]
	if err := json.Unmarshal([]byte(*jsonArgs), &toolArgs); err != nil {
		return "", nil, false, fmt.Errorf("--json: %w", err)
	}
	return tool, toolArgs, *verboseFlag, nil
}

// ---- REPL ------------------------------------------------------------------

func cmdRepl(args []string) error {
	set, verbose := fs("repl")
	connectTimeout := set.Int("connect-timeout", 90, "seconds to wait for the initial device connection")
	if err := set.Parse(args); err != nil {
		return err
	}
	// If a daemon is running, forward each line to its resident gateway instead
	// of spawning a second one (which would conflict on the gateway ports).
	if daemonAvailable() {
		fmt.Fprintln(os.Stderr, "repl: a daemon is running — forwarding commands to its gateway")
		return replLoop(nil, true, nil)
	}
	return withClient(*verbose, func(c *mcp.Client) error {
		fmt.Fprintln(os.Stderr, "gateway up; waiting for device to connect...")
		if err := waitConnected(c, time.Duration(*connectTimeout)*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v — commands may fail until the device connects\n", err)
		} else {
			fmt.Println("device connected ✓")
		}
		var toolNames []string
		if tools, err := c.ListTools(); err == nil {
			for _, t := range tools {
				toolNames = append(toolNames, t.Name)
			}
		}
		return replLoop(c, false, toolNames)
	})
}

// replLoop runs the interactive prompt. Local mode (remote=false) executes each
// line against c; remote mode forwards each line to the running daemon. quit /
// exit are always handled locally.
func replLoop(c *mcp.Client, remote bool, toolNames []string) error {
	fmt.Println("type 'help' for commands, 'quit' to exit  (Tab completes · ↑ history · Ctrl-R search)")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "stackchan> ",
		HistoryFile:       replHistoryFile(),
		AutoComplete:      buildCompleter(toolNames),
		InterruptPrompt:   "^C",
		EOFPrompt:         "quit",
		HistorySearchFold: true, // case-insensitive Ctrl-R
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		switch err {
		case readline.ErrInterrupt: // Ctrl-C: clear line, or exit if already empty
			if strings.TrimSpace(line) == "" {
				return nil
			}
			continue
		case io.EOF: // Ctrl-D
			return nil
		case nil:
		default:
			return err
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if w := strings.Fields(trimmed)[0]; w == "quit" || w == "exit" || w == "q" {
			return nil
		}
		if remote {
			forwarded, err := sendToDaemon(tokenize(trimmed))
			if !forwarded {
				err = errors.New("daemon not reachable (daemon gone?)")
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			continue
		}
		stop, derr := runLine(line, stdEnv(c))
		if derr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", derr)
		}
		if stop {
			return nil
		}
	}
}

// runLine executes one REPL/script line in e. It handles blank /
// '#'-comment lines, the meta commands (quit, help, repeat, source), and
// otherwise dispatches to a device/tool command. stop=true ends the REPL/script.
func runLine(line string, e cmdEnv) (stop bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false, nil
	}
	toks := tokenize(line)
	name, rest := toks[0], toks[1:]
	switch name {
	case "quit", "exit", "q":
		return true, nil
	case "help", "?":
		replHelp(e.out)
		return false, nil
	case "repeat":
		return false, doRepeat(rest, e)
	case "source", "run":
		return false, doSource(rest, e)
	default:
		return false, dispatch(name, rest, e)
	}
}

func doSleep(rest []string) error {
	if len(rest) < 1 {
		return fmt.Errorf("usage: sleep <ms>")
	}
	ms, err := strconv.Atoi(rest[0])
	if err != nil || ms < 0 {
		return fmt.Errorf("sleep: invalid ms %q", rest[0])
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	return nil
}

func doRepeat(rest []string, e cmdEnv) error {
	if len(rest) < 2 {
		return fmt.Errorf("usage: repeat <n> <command...>")
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		return fmt.Errorf("repeat: invalid count %q", rest[0])
	}
	cmdline := strings.Join(rest[1:], " ")
	for i := 0; i < n; i++ {
		stop, err := runLine(cmdline, e)
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}
	return nil
}

// doSource runs each line of a script file through runLine. Per-line errors are
// printed but do not abort the script.
func doSource(rest []string, e cmdEnv) error {
	set, _ := fs("source") // accepts (and ignores) --verbose, as cmdSource does
	if err := set.Parse(rest); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("usage: source <file>")
	}
	data, err := os.ReadFile(set.Arg(0))
	if err != nil {
		return err
	}
	if failed := runScript(string(data), e); failed > 0 {
		return fmt.Errorf("%s: %d line(s) failed", set.Arg(0), failed)
	}
	return nil
}

// runScript runs each line of script through runLine. A failed line is
// reported to e.errOut and counted; the script goes on.
func runScript(script string, e cmdEnv) (failed int) {
	for ln := range strings.SplitSeq(script, "\n") {
		stop, err := runLine(strings.TrimRight(ln, "\r"), e)
		if err != nil {
			failed++
			fmt.Fprintf(e.errOut, "error: %v\n", err)
		}
		if stop {
			break
		}
	}
	return failed
}

// cmdSource runs a script file in a single one-shot gateway session.
func cmdSource(args []string) error {
	set, verbose := fs("source")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("source requires a file")
	}
	file := set.Arg(0)
	return withDevice(*verbose, nil, func(c *mcp.Client) error {
		return doSource([]string{file}, stdEnv(c))
	})
}

func replHistoryFile() string {
	return filepath.Join(stackchanDir(), "repl_history")
}

// buildCompleter builds Tab completion for command names, avatar faces, common
// flags, and (for `call`) the given tool names (may be nil in remote/daemon mode).
func buildCompleter(toolNames []string) *readline.PrefixCompleter {
	faces := pcItems("idle", "happy", "thinking", "sad", "surprised", "embarrassed", "off")
	return readline.NewPrefixCompleter(
		readline.PcItem("status"),
		readline.PcItem("tools"),
		readline.PcItem("wait"),
		readline.PcItem("avatar", faces...),
		readline.PcItem("avatar-load"),
		readline.PcItem("mouth", pcItems("closed", "half", "open", "e", "u")...),
		readline.PcItem("blink", readline.PcItem("on"), readline.PcItem("off")),
		readline.PcItem("move-head", readline.PcItem("--yaw"), readline.PcItem("--pitch")),
		readline.PcItem("led", readline.PcItem("--index"), readline.PcItem("--r"), readline.PcItem("--g"), readline.PcItem("--b")),
		readline.PcItem("all-leds", readline.PcItem("--r"), readline.PcItem("--g"), readline.PcItem("--b")),
		readline.PcItem("clear-leds"),
		readline.PcItem("volume"),
		readline.PcItem("brightness"),
		readline.PcItem("say", readline.PcItem("--speaker"), readline.PcItem("--face"), readline.PcItem("--while-face")),
		readline.PcItem("listen", readline.PcItem("--sec"), readline.PcItem("--lang"), readline.PcItem("--model"), readline.PcItem("--motion")),
		readline.PcItem("voice"),
		readline.PcItem("pc"),
		readline.PcItem("ask", readline.PcItem("--quiet"), readline.PcItem("--open")),
		readline.PcItem("timer", readline.PcItem("cancel"), readline.PcItem("--say"), readline.PcItem("--quiet")),
		readline.PcItem("pomodoro", readline.PcItem("start"), readline.PcItem("stop"), readline.PcItem("ok")),
		readline.PcItem("photo", readline.PcItem("--question"), readline.PcItem("--open")),
		readline.PcItem("call", pcItems(toolNames...)...),
		readline.PcItem("sleep"),
		readline.PcItem("wakeup"),
		readline.PcItem("stop"),
		readline.PcItem("repeat"),
		readline.PcItem("source"),
		readline.PcItem("help"),
		readline.PcItem("quit"),
		readline.PcItem("exit"),
	)
}

func pcItems(names ...string) []readline.PrefixCompleterInterface {
	items := make([]readline.PrefixCompleterInterface, 0, len(names))
	for _, n := range names {
		items = append(items, readline.PcItem(n))
	}
	return items
}

func replHelp(w io.Writer) {
	fmt.Fprintln(w, `commands (device stays connected; runs instantly):
  status                         connection status
  tools                          list gateway tools
  avatar <face>                  idle|happy|thinking|sad|surprised|embarrassed|off
  avatar-load [--set NAME]       upload ~/.stackchan/avatar/*.png (or avatar-NAME/) as the face set
  mouth <shape>                  closed|half|open|e|u
  blink <on|off>
  move-head --yaw N --pitch N    yaw -90..90, pitch 5..85
  led --index N --r N --g N --b N
  all-leds --r N --g N --b N  |  clear-leds
  volume <0-100>  |  brightness <0-100>
  say [--speaker N] <text>       speak via VOICEVOX (3=ずんだもん, 8=春日部つむぎ)
  listen [--sec N]               record from the device mic and print the transcription
  voice [text]                   run the voice command rules on text (no text: list them)
  photo [--question "..."] [--open]   capture; --open shows the saved image
  call <tool> --json '{...}'     raw tool call
  sleep | wakeup                 sleep until wakeup or the end of the --sleep window (needs the daemon)
  sleep <ms>                     pause (for choreography)
  repeat <n> <command...>        run a command n times
  source <file>                  run commands from a file (# lines are comments)
  help | quit`)
}

// tokenize splits a REPL line into arguments, honoring single and double quotes
// so that `say "hello world"` and `call t --json '{"a":1}'` work as expected.
func tokenize(line string) []string {
	var toks []string
	var cur strings.Builder
	inSingle, inDouble, has := false, false, false
	flush := func() {
		if has {
			toks = append(toks, cur.String())
			cur.Reset()
			has = false
		}
	}
	for _, r := range line {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'':
			inSingle, has = true, true
		case r == '"':
			inDouble, has = true, true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return toks
}
