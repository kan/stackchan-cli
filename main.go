// Command stackchan-cli controls an M5 StackChan robot through the
// stackchan-mcp gateway. It spawns the gateway as a child process and drives it
// over stdio MCP (JSON-RPC 2.0): initialize -> tools/call.
//
// This is the stage-1 "one-shot" form: every command boots a fresh gateway,
// runs one tool, and exits. Device-requiring tools (move-head, avatar, ...)
// need a StackChan connected to the gateway's WebSocket; status/tools work
// without a device.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stackchan-cli/internal/mcp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "status":
		err = cmdStatus(args)
	case "tools":
		err = cmdTools(args)
	case "wait":
		err = cmdWait(args)
	case "move-head":
		err = cmdMoveHead(args)
	case "avatar":
		err = cmdAvatar(args)
	case "led":
		err = cmdLED(args)
	case "all-leds":
		err = cmdAllLEDs(args)
	case "say":
		err = cmdSay(args)
	case "photo":
		err = cmdPhoto(args)
	case "call":
		err = cmdCall(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stackchan-cli — control an M5 StackChan via the stackchan-mcp gateway

Usage:
  stackchan-cli <command> [flags]

Commands:
  status                 Show gateway/device connection status (no device needed)
  tools                  List the tools the gateway exposes (no device needed)
  wait [--timeout N]     Keep the gateway up and wait for the device to connect (mDNS)
  move-head --yaw N --pitch N
  avatar <face>          idle|happy|thinking|sad|surprised|embarrassed|off
  led --index N --r N --g N --b N
  all-leds --r N --g N --b N
  say <text>
  photo --question "..."
  call <tool> [--json '{...}']   Invoke any tool with raw JSON arguments

Environment:
  STACKCHAN_MCP_EXE   path to the gateway executable (default: PATH / ~/.local/bin)
  STACKCHAN_TOKEN     bearer token shared with the firmware (required by the gateway)
  VISION_HOST         this host's LAN IP, required for 'photo'

Global flags (place before the command's own flags):
  --verbose           forward the gateway's stderr logs
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
	return env
}

// callAndPrint runs one tool and prints its text result. When waitDevice is
// true it first blocks until the StackChan has (re)connected to the freshly
// spawned gateway, since one-shot mode starts a new gateway each invocation and
// the device needs a moment to reconnect to ws://<host>:8765/.
func callAndPrint(verbose bool, waitDevice bool, tool string, args map[string]any) error {
	return withClient(verbose, func(c *mcp.Client) error {
		if waitDevice {
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
		if strings.Contains(strings.ReplaceAll(res.Text(), " ", ""), "\"connected\":true") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device did not connect within %s", timeout)
		}
		time.Sleep(1 * time.Second)
	}
}

// fs builds a FlagSet with a shared --verbose flag and returns a pointer to it.
func fs(name string) (*flag.FlagSet, *bool) {
	set := flag.NewFlagSet(name, flag.ExitOnError)
	verbose := set.Bool("verbose", false, "forward gateway stderr logs")
	return set, verbose
}

// ---- commands --------------------------------------------------------------

func cmdStatus(args []string) error {
	set, verbose := fs("status")
	_ = set.Parse(args)
	return callAndPrint(*verbose, false, "get_status", map[string]any{})
}

func cmdTools(args []string) error {
	set, verbose := fs("tools")
	_ = set.Parse(args)
	return withClient(*verbose, func(c *mcp.Client) error {
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

func cmdWait(args []string) error {
	set, verbose := fs("wait")
	timeout := set.Int("timeout", 60, "seconds to wait for the device to connect")
	_ = set.Parse(args)
	return withClient(*verbose, func(c *mcp.Client) error {
		deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
		for {
			res, err := c.CallTool("get_status", map[string]any{})
			if err != nil {
				return err
			}
			txt := strings.Join(strings.Fields(res.Text()), " ")
			fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), txt)
			if strings.Contains(strings.ReplaceAll(txt, " ", ""), "\"connected\":true") {
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

func cmdMoveHead(args []string) error {
	set, verbose := fs("move-head")
	yaw := set.Int("yaw", 0, "horizontal angle, -90..90")
	pitch := set.Int("pitch", 45, "vertical angle, 5..85")
	_ = set.Parse(args)
	return callAndPrint(*verbose, true, "move_head", map[string]any{"yaw": *yaw, "pitch": *pitch})
}

func cmdAvatar(args []string) error {
	set, verbose := fs("avatar")
	_ = set.Parse(args)
	if set.NArg() < 1 {
		return fmt.Errorf("avatar requires a face (idle|happy|thinking|sad|surprised|embarrassed|off)")
	}
	return callAndPrint(*verbose, true, "set_avatar", map[string]any{"face": set.Arg(0)})
}

func cmdLED(args []string) error {
	set, verbose := fs("led")
	index := set.Int("index", 0, "LED index 0..11")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	_ = set.Parse(args)
	return callAndPrint(*verbose, true, "set_led", map[string]any{"index": *index, "r": *r, "g": *g, "b": *b})
}

func cmdAllLEDs(args []string) error {
	set, verbose := fs("all-leds")
	r := set.Int("r", 0, "red 0..255")
	g := set.Int("g", 0, "green 0..255")
	b := set.Int("b", 0, "blue 0..255")
	_ = set.Parse(args)
	return callAndPrint(*verbose, true, "set_all_leds", map[string]any{"r": *r, "g": *g, "b": *b})
}

func cmdSay(args []string) error {
	set, verbose := fs("say")
	_ = set.Parse(args)
	if set.NArg() < 1 {
		return fmt.Errorf("say requires text")
	}
	return callAndPrint(*verbose, true, "say", map[string]any{"text": set.Arg(0)})
}

func cmdPhoto(args []string) error {
	set, verbose := fs("photo")
	question := set.String("question", "What do you see?", "question to ask about the photo")
	_ = set.Parse(args)
	return callAndPrint(*verbose, true, "take_photo", map[string]any{"question": *question})
}

func cmdCall(args []string) error {
	set, verbose := fs("call")
	jsonArgs := set.String("json", "{}", "tool arguments as a JSON object")
	_ = set.Parse(args)
	if set.NArg() < 1 {
		return fmt.Errorf("call requires a tool name")
	}
	var toolArgs map[string]any
	if err := json.Unmarshal([]byte(*jsonArgs), &toolArgs); err != nil {
		return fmt.Errorf("--json: %w", err)
	}
	return callAndPrint(*verbose, true, set.Arg(0), toolArgs)
}
