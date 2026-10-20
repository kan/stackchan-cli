package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAskHelperProcess is not a test: it is the fake claude the tests below
// start (the test binary re-run with ASK_HELPER set). It records its
// arguments and working directory, then answers as the mode says.
func TestAskHelperProcess(t *testing.T) {
	mode := os.Getenv("ASK_HELPER")
	if mode == "" {
		return
	}
	var args []string
	_ = json.Unmarshal([]byte(os.Getenv("ASK_ARGS")), &args)
	wd, _ := os.Getwd()
	rec, _ := json.Marshal(map[string]any{"args": args, "dir": wd, "config": os.Getenv("CLAUDE_CONFIG_DIR")})
	f, _ := os.OpenFile(os.Getenv("ASK_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintf(f, "%s\n", rec)
	f.Close()
	resumed := slices.Contains(args, "--resume")
	switch {
	case mode == "hang":
		time.Sleep(time.Minute)
	case mode == "error", mode == "no-resume" && resumed:
		fmt.Println(`{"is_error": true, "subtype": "error_during_execution", "result": ""}`)
	case mode == "garbage":
		fmt.Println("not json")
	default:
		q := questionOf(args)
		out, _ := json.Marshal(map[string]any{"result": "答え: " + q, "session_id": fmt.Sprintf("s%d", time.Now().UnixNano()), "total_cost_usd": 0.01})
		fmt.Println(string(out))
	}
	os.Exit(0)
}

// fakeClaude makes ask run the helper in mode and returns a reader of the
// calls it got.
func fakeClaude(t *testing.T, mode string) func() []map[string]any {
	t.Helper()
	fakeHome(t)
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	old := claudeCommand
	claudeCommand = func(ctx context.Context, args []string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAskHelperProcess$")
		a, _ := json.Marshal(args)
		cmd.Env = append(os.Environ(), "ASK_HELPER="+mode, "ASK_ARGS="+string(a), "ASK_LOG="+log)
		return cmd
	}
	t.Cleanup(func() { claudeCommand = old })
	return func() []map[string]any {
		data, _ := os.ReadFile(log)
		var calls []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				calls = append(calls, m)
			}
		}
		return calls
	}
}

// askQ is ask with its notes dropped.
func askQ(question string, c askConfig, now time.Time) (claudeReply, error) {
	return ask(question, c, now, io.Discard)
}

// questionOf is the question in claude's arguments: the last one, after "--".
func questionOf(args []string) string {
	if len(args) < 2 || args[len(args)-2] != "--" {
		return ""
	}
	return args[len(args)-1]
}

func argsOf(call map[string]any) []string {
	var out []string
	for _, a := range call["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func TestAskContinuesWithinMemory(t *testing.T) {
	calls := fakeClaude(t, "ok")
	c := defaultAskConfig()
	c.ConfigDir = filepath.Join(t.TempDir(), "claude-config")
	now := time.Now()
	r, err := askQ("東京タワーってなに", c, now)
	if err != nil || r.Result != "答え: 東京タワーってなに" {
		t.Fatalf("ask = %+v, %v", r, err)
	}
	if _, err := askQ("それっていつできたの", c, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	saveSession("old", time.Now().Add(-10*time.Minute)) // the last answer is old
	if _, err := askQ("こんにちは", c, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("%d calls, want 3", len(got))
	}
	first, second, third := argsOf(got[0]), argsOf(got[1]), argsOf(got[2])
	if slices.Contains(first, "--resume") || !slices.Contains(second, "--resume") || slices.Contains(third, "--resume") {
		t.Errorf("--resume: want only the second call to continue\n%q\n%q\n%q", first, second, third)
	}
	// Only web search; no MCP servers or personal settings; the persona as
	// the whole system prompt; run in ~/.stackchan/ask with the config dir.
	for _, want := range [][]string{{"--tools", "WebSearch,WebFetch"}, {"--setting-sources", ""}, {"--model", "sonnet"}, {"--system-prompt", defaultAskSystem}} {
		i := slices.Index(first, want[0])
		if i < 0 || first[i+1] != want[1] {
			t.Errorf("args lack %q %q: %q", want[0], want[1], first)
		}
	}
	if !slices.Contains(first, "--strict-mcp-config") {
		t.Errorf("args lack --strict-mcp-config: %q", first)
	}
	if got[0]["dir"] != askDir() || got[0]["config"] != c.ConfigDir {
		t.Errorf("dir %v, config %v; want %s, %s", got[0]["dir"], got[0]["config"], askDir(), c.ConfigDir)
	}
}

// A conversation that cannot be continued is asked anew.
func TestAskResumeFailsAsksAnew(t *testing.T) {
	calls := fakeClaude(t, "no-resume")
	c := defaultAskConfig()
	saveSession("old", time.Now()) // e.g. from an earlier process
	r, err := askQ("こんにちは", c, time.Now())
	if err != nil || r.Result == "" {
		t.Fatalf("ask = %+v, %v", r, err)
	}
	if got := calls(); len(got) != 2 || !slices.Contains(argsOf(got[0]), "--resume") || slices.Contains(argsOf(got[1]), "--resume") {
		t.Errorf("calls: %v, want a resume, then a new one", got)
	}
	// Both calls count toward the daily limit.
	if data, _ := os.ReadFile(askCountPath()); !strings.HasSuffix(strings.TrimSpace(string(data)), " 2") {
		t.Errorf("count = %q, want 2", data)
	}
}

// A question starting with "-" goes after "--", not taken for an option.
func TestAskQuestionAfterDashDash(t *testing.T) {
	calls := fakeClaude(t, "ok")
	if _, err := askQ("-5度って寒い？", defaultAskConfig(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if q := questionOf(argsOf(calls()[0])); q != "-5度って寒い？" {
		t.Errorf("question = %q", q)
	}
}

// A .cmd wrapper is refused: cmd.exe would mangle the arguments.
func TestAskRefusesCmdWrapper(t *testing.T) {
	fakeHome(t)
	old := claudeCommand
	claudeCommand = func(ctx context.Context, args []string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "claude.cmd"), args...)
	}
	t.Cleanup(func() { claudeCommand = old })
	if _, err := askQ("こんにちは", defaultAskConfig(), time.Now()); err == nil || !strings.Contains(err.Error(), "wrapper") {
		t.Errorf("err = %v, want the wrapper refused", err)
	}
}

func TestAskFailures(t *testing.T) {
	c := defaultAskConfig()
	c.Timeout = time.Second
	for mode, want := range map[string]error{"hang": errAskTimeout, "error": nil, "garbage": nil} {
		fakeClaude(t, mode)
		start := time.Now()
		_, err := askQ("こんにちは", c, start)
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("%s: err = %v, want %v", mode, err, want)
		}
		if mode == "hang" && time.Since(start) > 10*time.Second {
			t.Errorf("hang: took %s", time.Since(start))
		}
		if askFailed(err) == "" {
			t.Errorf("%s: nothing to say", mode)
		}
	}
}

func TestAskDailyLimit(t *testing.T) {
	calls := fakeClaude(t, "ok")
	c := defaultAskConfig()
	c.Limit = 2
	day := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	for i := range 3 {
		_, err := askQ("こんにちは", c, day.Add(time.Duration(i)*time.Minute))
		if (i < 2) != (err == nil) || (i == 2 && !errors.Is(err, errAskLimit)) {
			t.Errorf("call %d: %v", i+1, err)
		}
	}
	if n := len(calls()); n != 2 {
		t.Errorf("claude ran %d times, want 2", n)
	}
	if _, err := askQ("こんにちは", c, day.AddDate(0, 0, 1)); err != nil {
		t.Errorf("the next day: %v", err)
	}
	if askFailed(errAskLimit) != "今日はもう調べられないよ。また明日ね。" {
		t.Error("limit message")
	}
}

func TestParseAskConfig(t *testing.T) {
	c, err := parseAskConfig(defaultAskFile)
	if err != nil || c != defaultAskConfig() {
		t.Errorf("the seeded file = %+v, %v; want the defaults", c, err)
	}
	c, err = parseAskConfig("model = opus\nlimit = 0\ntimeout = 60\nmemory = 0\nconfig_dir = ~/.claude-ai\n")
	home, _ := os.UserHomeDir()
	if err != nil || c.Model != "opus" || c.Limit != 0 || c.Timeout != time.Minute || c.Memory != 0 || c.ConfigDir != filepath.Join(home, ".claude-ai") {
		t.Errorf("parsed = %+v, %v", c, err)
	}
	for _, bad := range []string{"timeout = 0", "limit = -1", "model =", "color = red"} {
		if c, err := parseAskConfig(bad); err == nil || c != defaultAskConfig() {
			t.Errorf("%q: %+v, %v; want an error and the defaults", bad, c, err)
		}
	}
}

func TestVoiceAskRule(t *testing.T) {
	vr, err := parseVoiceRules(defaultVoiceCommands)
	if err != nil {
		t.Fatal(err)
	}
	for heard, want := range map[string]string{
		"スタックチャン、東京タワーってなに？": `ask "スタックチャン、東京タワーってなに？"`,
		`光合成について"教えて"`:       `ask "光合成について教えて"`,
		"明日の天気を教えて":          `say "{weather_tomorrow}"`, // the built-in answers first
		"ポモドーロってなに？":         `ask "ポモドーロってなに？"`,         // not a pomodoro start
		"1時間って何分？":           `ask "1時間って何分？"`,
	} {
		if got, _ := vr.match(heard); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s -> %q, want %q", heard, got, want)
		}
	}
	if got := heardForCommands("a\";say x\nb"); got != "a、say x b" {
		t.Errorf("heardForCommands did not defuse quotes, ; and newlines: %q", got)
	}
}

// The conversation to continue is kept in a file, so a one-shot ask (a new
// process) continues it too; a failure forgets it.
func TestAskSessionFile(t *testing.T) {
	fakeHome(t)
	c := defaultAskConfig()
	now := time.Now()
	if lastSession(c, now) != "" {
		t.Error("a session with no file")
	}
	saveSession("s1", now)
	if got := lastSession(c, now.Add(4*time.Minute)); got != "s1" {
		t.Errorf("4 minutes later: %q, want s1", got)
	}
	if got := lastSession(c, now.Add(6*time.Minute)); got != "" {
		t.Errorf("6 minutes later: %q, want none", got)
	}
	c.Memory = 0
	if got := lastSession(c, now); got != "" {
		t.Errorf("memory 0: %q, want none", got)
	}
	saveSession("", time.Time{})
	if got := lastSession(defaultAskConfig(), now); got != "" {
		t.Errorf("after a failure: %q, want none", got)
	}
}

func TestCmdAskQuiet(t *testing.T) {
	fakeClaude(t, "ok")
	var out, errOut bytes.Buffer
	if err := cmdAsk([]string{"--quiet", "こんにちは"}, cmdEnv{out: &out, errOut: &errOut}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "答え: こんにちは" {
		t.Errorf("out = %q", out.String())
	}
	if err := cmdAsk([]string{"--quiet"}, cmdEnv{out: &out, errOut: &errOut}); err == nil {
		t.Error("an empty question was accepted")
	}
}
