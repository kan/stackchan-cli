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
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"stackchan-cli/internal/mcp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

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
	}

	// one-shot: nil client => each handler spawns its own gateway.
	if err := dispatch(cmd, args, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes a command name to its handler. c is nil in one-shot mode
// (handlers spawn a fresh gateway) or a live client in REPL mode (reused, so
// the gateway and device connection persist between commands).
func dispatch(cmd string, args []string, c *mcp.Client) error {
	switch cmd {
	case "status":
		return cmdStatus(args, c)
	case "tools":
		return cmdTools(args, c)
	case "wait":
		return cmdWait(args, c)
	case "move-head":
		return cmdMoveHead(args, c)
	case "avatar":
		return cmdAvatar(args, c)
	case "led":
		return cmdLED(args, c)
	case "all-leds":
		return cmdAllLEDs(args, c)
	case "say":
		return cmdSay(args, c)
	case "photo":
		return cmdPhoto(args, c)
	case "call":
		return cmdCall(args, c)
	default:
		return fmt.Errorf("unknown command: %s (try 'help')", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stackchan-cli — control an M5 StackChan via the stackchan-mcp gateway

Usage:
  stackchan-cli <command> [flags]
  stackchan-cli repl                  Interactive session (gateway stays resident; fast)

Commands:
  status                 Show gateway/device connection status (no device needed)
  tools                  List the tools the gateway exposes (no device needed)
  wait [--timeout N]     Keep the gateway up and wait for the device to connect
  move-head --yaw N --pitch N
  avatar <face>          idle|happy|thinking|sad|surprised|embarrassed|off
  led --index N --r N --g N --b N
  all-leds --r N --g N --b N
  say <text>
  photo [--question "..."] [--open]   capture a photo; saved to ~/.stackchan/captures
  call <tool> [--json '{...}']   Invoke any tool with raw JSON arguments

Environment:
  STACKCHAN_MCP_EXE   path to the gateway executable (default: PATH / ~/.local/bin)
  STACKCHAN_TOKEN     bearer token shared with the firmware (empty = no auth)
  VISION_HOST         this host's LAN IP for 'photo' (auto-detected if unset)

Per-command flags:
  --verbose           forward the gateway's stderr logs (one-shot mode)

In one-shot mode every command restarts the gateway, so device commands wait up
to 90s for the StackChan to reconnect. Use 'repl' for snappy repeated control.
`)
}

// ---- shared plumbing -------------------------------------------------------

// withClient boots a gateway, initializes the session, runs fn, then shuts down.
func withClient(verbose bool, fn func(*mcp.Client) error) error {
	exe := gatewayPath()
	env := gatewayEnv()

	var stderr io.Writer
	if verbose {
		stderr = os.Stderr
	}

	c, err := mcp.Start(exe, []string{"serve"}, env, stderr)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return fn(c)
}

// withClientOrReuse runs fn against an existing client (REPL mode, c != nil) or
// against a freshly spawned one-shot gateway (c == nil).
func withClientOrReuse(verbose bool, c *mcp.Client, fn func(*mcp.Client) error) error {
	if c != nil {
		return fn(c)
	}
	return withClient(verbose, fn)
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
	return env
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
// (outer == nil) it spawns a gateway and, when waitDevice is set, blocks until
// the StackChan reconnects. In REPL mode (outer != nil) it reuses the resident
// client and skips the wait, since the device stays connected between commands.
func callAndPrint(verbose, waitDevice bool, tool string, args map[string]any, outer *mcp.Client) error {
	oneShot := outer == nil
	return withClientOrReuse(verbose, outer, func(c *mcp.Client) error {
		if waitDevice && oneShot {
			fmt.Fprintln(os.Stderr, "waiting for device to connect...")
			if err := waitConnected(c, 90*time.Second); err != nil {
				return err
			}
		}
		res, err := c.CallTool(tool, args)
		if err != nil {
			return err
		}
		fmt.Println(res.Text())
		if res.IsError {
			return fmt.Errorf("tool %s reported an error", tool)
		}
		return nil
	})
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
	return strings.Contains(strings.ReplaceAll(statusText, " ", ""), "\"connected\":true")
}

// fs builds a FlagSet with a shared --verbose flag. ContinueOnError keeps a bad
// flag from calling os.Exit (which would kill the REPL).
func fs(name string) (*flag.FlagSet, *bool) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	verbose := set.Bool("verbose", false, "forward gateway stderr logs")
	return set, verbose
}

// ---- commands --------------------------------------------------------------

func cmdStatus(args []string, c *mcp.Client) error {
	set, verbose := fs("status")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, false, "get_status", map[string]any{}, c)
}

func cmdTools(args []string, c *mcp.Client) error {
	set, verbose := fs("tools")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withClientOrReuse(*verbose, c, func(c *mcp.Client) error {
		tools, err := c.ListTools()
		if err != nil {
			return err
		}
		for _, t := range tools {
			var schema struct {
				Required []string `json:"required"`
			}
			_ = json.Unmarshal(t.InputSchema, &schema)
			fmt.Printf("%-24s %v\n", t.Name, schema.Required)
		}
		fmt.Fprintf(os.Stderr, "\n%d tools\n", len(tools))
		return nil
	})
}

func cmdWait(args []string, c *mcp.Client) error {
	set, verbose := fs("wait")
	timeout := set.Int("timeout", 60, "seconds to wait for the device to connect")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withClientOrReuse(*verbose, c, func(c *mcp.Client) error {
		deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
		for {
			res, err := c.CallTool("get_status", map[string]any{})
			if err != nil {
				return err
			}
			txt := strings.Join(strings.Fields(res.Text()), " ")
			fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), txt)
			if isConnected(txt) {
				fmt.Println("device connected ✓")
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("device did not connect within %ds (check Wi-Fi, mDNS, and Windows Firewall on port 8765)", *timeout)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func cmdMoveHead(args []string, c *mcp.Client) error {
	set, verbose := fs("move-head")
	yaw := set.Int("yaw", 0, "horizontal angle, -90..90")
	pitch := set.Int("pitch", 45, "vertical angle, 5..85")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "move_head", map[string]any{"yaw": *yaw, "pitch": *pitch}, c)
}

func cmdAvatar(args []string, c *mcp.Client) error {
	set, verbose := fs("avatar")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("avatar requires a face (idle|happy|thinking|sad|surprised|embarrassed|off)")
	}
	return callAndPrint(*verbose, true, "set_avatar", map[string]any{"face": set.Arg(0)}, c)
}

func cmdLED(args []string, c *mcp.Client) error {
	set, verbose := fs("led")
	index := set.Int("index", 0, "LED index 0..11")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_led", map[string]any{"index": *index, "r": *r, "g": *g, "b": *b}, c)
}

func cmdAllLEDs(args []string, c *mcp.Client) error {
	set, verbose := fs("all-leds")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	if err := set.Parse(args); err != nil {
		return err
	}
	return callAndPrint(*verbose, true, "set_all_leds", map[string]any{"r": *r, "g": *g, "b": *b}, c)
}

func cmdSay(args []string, c *mcp.Client) error {
	set, verbose := fs("say")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("say requires text")
	}
	return callAndPrint(*verbose, true, "say", map[string]any{"text": strings.Join(set.Args(), " ")}, c)
}

func cmdPhoto(args []string, c *mcp.Client) error {
	set, verbose := fs("photo")
	question := set.String("question", "What do you see?", "question to ask about the photo")
	open := set.Bool("open", false, "open the saved image in the default viewer")
	if err := set.Parse(args); err != nil {
		return err
	}
	oneShot := c == nil
	return withClientOrReuse(*verbose, c, func(c *mcp.Client) error {
		if oneShot {
			fmt.Fprintln(os.Stderr, "waiting for device to connect...")
			if err := waitConnected(c, 90*time.Second); err != nil {
				return err
			}
		}
		res, err := c.CallTool("take_photo", map[string]any{"question": *question})
		if err != nil {
			return err
		}
		fmt.Println(res.Text())
		if res.IsError {
			return fmt.Errorf("take_photo reported an error")
		}
		path := savedImagePath(res.Text())
		if path == "" {
			fmt.Fprintf(os.Stderr, "(image not located; check %s)\n", capturesDir())
			return nil
		}
		fmt.Printf("photo saved: %s\n", path)
		if *open {
			openInViewer(path)
		}
		return nil
	})
}

func capturesDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stackchan", "captures")
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

// openInViewer opens a file with the OS default handler (Windows shell).
func openInViewer(path string) {
	cmd := exec.Command("cmd", "/c", "start", "", path)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open %s: %v\n", path, err)
	}
}

func cmdCall(args []string, c *mcp.Client) error {
	set, verbose := fs("call")
	jsonArgs := set.String("json", "{}", "tool arguments as a JSON object")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return fmt.Errorf("call requires a tool name")
	}
	var toolArgs map[string]any
	if err := json.Unmarshal([]byte(*jsonArgs), &toolArgs); err != nil {
		return fmt.Errorf("--json: %w", err)
	}
	return callAndPrint(*verbose, true, set.Arg(0), toolArgs, c)
}

// ---- REPL ------------------------------------------------------------------

func cmdRepl(args []string) error {
	set, verbose := fs("repl")
	connectTimeout := set.Int("connect-timeout", 90, "seconds to wait for the initial device connection")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withClient(*verbose, func(c *mcp.Client) error {
		fmt.Fprintln(os.Stderr, "gateway up; waiting for device to connect...")
		if err := waitConnected(c, time.Duration(*connectTimeout)*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v — commands may fail until the device connects\n", err)
		} else {
			fmt.Println("device connected ✓")
		}
		fmt.Println("type 'help' for commands, 'quit' to exit  (Tab completes · ↑ history · Ctrl-R search)")

		rl, err := readline.NewEx(&readline.Config{
			Prompt:            "stackchan> ",
			HistoryFile:       replHistoryFile(),
			AutoComplete:      buildCompleter(c),
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

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			toks := tokenize(line)
			name, rest := toks[0], toks[1:]
			switch name {
			case "quit", "exit", "q":
				return nil
			case "help", "?":
				replHelp()
			default:
				if err := dispatch(name, rest, c); err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				}
			}
		}
	})
}

func replHistoryFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stackchan", "repl_history")
}

// buildCompleter builds Tab completion for command names, avatar faces, common
// flags, and (for `call`) the gateway's advertised tool names.
func buildCompleter(c *mcp.Client) *readline.PrefixCompleter {
	faces := pcItems("idle", "happy", "thinking", "sad", "surprised", "embarrassed", "off")

	var toolNames []string
	if tools, err := c.ListTools(); err == nil {
		for _, t := range tools {
			toolNames = append(toolNames, t.Name)
		}
	}

	return readline.NewPrefixCompleter(
		readline.PcItem("status"),
		readline.PcItem("tools"),
		readline.PcItem("wait"),
		readline.PcItem("avatar", faces...),
		readline.PcItem("move-head", readline.PcItem("--yaw"), readline.PcItem("--pitch")),
		readline.PcItem("led", readline.PcItem("--index"), readline.PcItem("--r"), readline.PcItem("--g"), readline.PcItem("--b")),
		readline.PcItem("all-leds", readline.PcItem("--r"), readline.PcItem("--g"), readline.PcItem("--b")),
		readline.PcItem("say"),
		readline.PcItem("photo", readline.PcItem("--question"), readline.PcItem("--open")),
		readline.PcItem("call", pcItems(toolNames...)...),
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

func replHelp() {
	fmt.Println(`commands (device stays connected; runs instantly):
  status                         connection status
  tools                          list gateway tools
  avatar <face>                  idle|happy|thinking|sad|surprised|embarrassed|off
  move-head --yaw N --pitch N    yaw -90..90, pitch 5..85
  led --index N --r N --g N --b N
  all-leds --r N --g N --b N
  say <text>                     (needs gateway TTS extra)
  photo [--question "..."] [--open]   capture; --open shows the saved image
  call <tool> --json '{...}'     raw tool call
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
