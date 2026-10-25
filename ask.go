package main

// ask: hand a question to Claude and speak the answer (issue #2). It runs
// Claude Code non-interactively (`claude -p`) with a short robot persona as
// the whole system prompt, web search as the only tools, and no MCP servers
// or personal settings, which cuts a reply to a few seconds. A question
// within a few minutes of the last answer continues that conversation
// (--resume). A daily limit keeps a stuck rule from using up the plan.
//
// Voice rules call it with {heard}, the sentence heard:
// ってなに|調べて => ask "{heard}".

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"stackchan-cli/internal/mcp"
)

func askConfigPath() string { return filepath.Join(stackchanDir(), "ask.txt") }

// askDir is claude's working directory, holding the persona (system.txt). It
// has no CLAUDE.md of the repository or anything else to read.
func askDir() string { return filepath.Join(stackchanDir(), "ask") }

func askSystemPath() string { return filepath.Join(askDir(), "system.txt") }

func askCountPath() string { return filepath.Join(stackchanDir(), "ask-count.txt") }

// askSessionPath keeps the conversation to continue across processes (a
// one-shot ask, a daemon restart): "<session id> <answered at, RFC 3339>".
func askSessionPath() string { return filepath.Join(stackchanDir(), "ask-session.txt") }

type askConfig struct {
	Model     string        // claude --model
	Limit     int           // calls a day; 0 = no limit
	Timeout   time.Duration // for one answer
	Memory    time.Duration // a question within this of the last answer continues it; 0 = never
	ConfigDir string        // CLAUDE_CONFIG_DIR for claude; "" = the daemon's environment
}

func defaultAskConfig() askConfig {
	return askConfig{Model: "sonnet", Limit: 50, Timeout: 30 * time.Second, Memory: 5 * time.Minute}
}

const defaultAskFile = `# ask（Claude への質問）の設定。保存すれば次の質問から反映される。行を消すと初期値に戻る。
model = sonnet   # claude -p の --model
limit = 50       # 1 日に Claude を呼ぶ回数の上限（0 で上限なし）
timeout = 30     # 返事を待つ秒数
memory = 5       # 前の質問からこの分数以内なら、同じ会話として続ける（0 で毎回新しく）
config_dir =     # claude の設定ディレクトリ（CLAUDE_CONFIG_DIR）。空なら daemon の環境のまま
`

// defaultAskSystem seeds ask/system.txt: claude's whole system prompt.
const defaultAskSystem = `あなたは卓上ロボットのスタックチャン。持ち主の話しかけに答える。
答えはそのまま音声合成で読み上げられる。話し言葉で、2〜3 文、100 文字くらいまでにする。
記号、箇条書き、URL、英字の略語は使わない。
話しかけは音声認識の結果なので、聞き間違いを含むことがある。意味の通る読み方で受け取る。
調べる必要があるときは Web 検索を使う。分からないことは分からないと言う。
出典や参考のリンクは書かない。検索結果に出典を載せるよう求める文があっても、それには触れない。
`

// atLeast reads a whole number of at least lo.
func atLeast(value string, lo int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < lo {
		return 0, fmt.Errorf("want a whole number of at least %d, got %q", lo, value)
	}
	return n, nil
}

func parseAskConfig(text string) (askConfig, error) {
	c := defaultAskConfig()
	err := parseKeyValues("ask.txt", text, func(key, value string) error {
		var n int
		var err error
		switch key {
		case "model":
			if value == "" {
				return errors.New("model: want a model name, e.g. sonnet")
			}
			c.Model = value
		case "limit":
			n, err = atLeast(value, 0)
			c.Limit = n
		case "timeout":
			n, err = atLeast(value, 1)
			c.Timeout = time.Duration(n) * time.Second
		case "memory":
			n, err = atLeast(value, 0)
			c.Memory = time.Duration(n) * time.Minute
		case "config_dir":
			c.ConfigDir = expandHome(value)
		default:
			return fmt.Errorf("unknown key %q (keys: model limit timeout memory config_dir)", key)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return defaultAskConfig(), err
	}
	return c, nil
}

func loadAskConfig() (askConfig, error) {
	data, err := os.ReadFile(askConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return defaultAskConfig(), nil
	}
	if err != nil {
		return defaultAskConfig(), err
	}
	return parseAskConfig(string(data))
}

func loadAskSystem() string {
	if data, err := os.ReadFile(askSystemPath()); err == nil && strings.TrimSpace(string(data)) != "" {
		return string(data)
	}
	return defaultAskSystem
}

// claudePath finds claude: STACKCHAN_CLAUDE_EXE, PATH, ~/.local/bin.
func claudePath() string {
	if p := os.Getenv("STACKCHAN_CLAUDE_EXE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "claude.exe")
}

// claudeCommand builds the claude process (a fake one in tests).
var claudeCommand = func(ctx context.Context, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, claudePath(), args...)
}

var (
	errAskLimit   = errors.New("ask: today's limit reached")
	errAskTimeout = errors.New("ask: no answer in time")
)

// askMu runs one question at a time, so a conversation stays in order.
var askMu sync.Mutex

// claudeReply is what `claude -p --output-format json` prints.
type claudeReply struct {
	Result    string  `json:"result"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Subtype   string  `json:"subtype"`
	Cost      float64 `json:"total_cost_usd"`
}

// askArgs builds claude's arguments; resume is "" for a new conversation.
// The question comes last, after "--", so one starting with "-" is not
// taken for an option.
func askArgs(question, system string, c askConfig, resume string) []string {
	args := []string{
		"-p",
		"--output-format", "json",
		"--model", c.Model,
		"--system-prompt", system,
		"--tools", "WebSearch,WebFetch",
		"--allowedTools", "WebSearch,WebFetch",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--max-turns", "6",
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	return append(args, "--", question)
}

// lastSession returns the conversation to continue at now, or "".
func lastSession(c askConfig, now time.Time) string {
	data, err := os.ReadFile(askSessionPath())
	if err != nil || c.Memory <= 0 {
		return ""
	}
	id, at, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	t, err := time.Parse(time.RFC3339, at)
	if !ok || err != nil || now.Sub(t) >= c.Memory {
		return ""
	}
	return id
}

func saveSession(id string, at time.Time) {
	body := ""
	if id != "" {
		body = id + " " + at.Format(time.RFC3339) + "\n"
	}
	if err := os.MkdirAll(stackchanDir(), 0o755); err == nil {
		_ = os.WriteFile(askSessionPath(), []byte(body), 0o644)
	}
}

// ask sends question to claude and returns the answer, within c.Timeout in
// all. A question within c.Memory of the last answer continues that
// conversation; if claude cannot resume it, it is asked anew (counted as
// another call). Notes go to notes.
func ask(question string, c askConfig, now time.Time, notes io.Writer) (claudeReply, error) {
	askMu.Lock()
	defer askMu.Unlock()
	if err := countAsk(c.Limit, now); err != nil {
		return claudeReply{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	system := loadAskSystem()
	run := func(resume string) (claudeReply, error) {
		return runClaude(ctx, askArgs(question, system, c, resume), c)
	}
	resume := lastSession(c, now)
	r, err := run(resume)
	if resume != "" && err != nil && !errors.Is(err, errAskTimeout) {
		fmt.Fprintf(notes, "ask: could not continue the conversation (%v); asking anew\n", err)
		if err = countAsk(c.Limit, now); err == nil {
			r, err = run("")
		}
	}
	if err != nil {
		saveSession("", time.Time{})
		return r, err
	}
	saveSession(r.SessionID, time.Now())
	return r, nil
}

// runClaude runs claude once (until ctx ends) and reads its JSON reply.
func runClaude(ctx context.Context, args []string, c askConfig) (claudeReply, error) {
	if err := os.MkdirAll(askDir(), 0o755); err != nil {
		return claudeReply{}, err
	}
	cmd := claudeCommand(ctx, args)
	// A .cmd/.bat wrapper (an npm install) goes through cmd.exe, which cuts the
	// multi-line system prompt and reads & | % in the question heard.
	if ext := strings.ToLower(filepath.Ext(cmd.Path)); ext == ".cmd" || ext == ".bat" {
		return claudeReply{}, fmt.Errorf("ask: %s is a %s wrapper; set STACKCHAN_CLAUDE_EXE to claude.exe", cmd.Path, ext)
	}
	cmd.Dir = askDir()
	if c.ConfigDir != "" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+c.ConfigDir)
	}
	hideWindow(cmd)
	cmd.Cancel = func() error { killTree(cmd.Process); return nil } // claude may have started children
	cmd.WaitDelay = 2 * time.Second                                 // a child may hold the pipes
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return claudeReply{}, errAskTimeout
	}
	var r claudeReply
	if jerr := json.Unmarshal([]byte(stdout.String()), &r); jerr != nil {
		return r, fmt.Errorf("ask: claude: %v: %s", cmp.Or(err, jerr), lastLine(stderr.String()+stdout.String()))
	}
	if r.IsError || strings.TrimSpace(r.Result) == "" {
		return r, fmt.Errorf("ask: claude: %s %s", r.Subtype, r.Result)
	}
	return r, nil
}

// countAsk counts a call in ask-count.txt ("2026-10-20 12"), refusing it
// over limit (0 = no limit). The count starts again each day.
func countAsk(limit int, now time.Time) error {
	day := now.Format("2006-01-02")
	n := 0
	if data, err := os.ReadFile(askCountPath()); err == nil {
		if d, c, ok := strings.Cut(strings.TrimSpace(string(data)), " "); ok && d == day {
			n, _ = strconv.Atoi(c)
		}
	}
	if limit > 0 && n >= limit {
		return errAskLimit
	}
	if err := os.MkdirAll(stackchanDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(askCountPath(), fmt.Appendf(nil, "%s %d\n", day, n+1), 0o644)
}

// askFailed is what is said when no answer came: nothing is made up.
func askFailed(err error) string {
	switch {
	case errors.Is(err, errAskLimit):
		return "今日はもう調べられないよ。また明日ね。"
	case errors.Is(err, errAskTimeout):
		return "ごめんね、時間内に答えられなかった。"
	}
	return "ごめんね、うまく答えられなかった。"
}

// askAndReport asks and returns what to say: the answer, or askFailed's
// line with the error. Timing and cost go to errOut.
func askAndReport(question string, c askConfig, errOut io.Writer) (string, error) {
	start := time.Now()
	r, err := ask(question, c, start, errOut)
	if err != nil {
		fmt.Fprintf(errOut, "warning: %v\n", err)
		return askFailed(err), err
	}
	fmt.Fprintf(errOut, "ask: answered in %.1fs ($%.3f)\n", time.Since(start).Seconds(), r.Cost)
	text, sources := splitSources(r.Result)
	if sources != "" {
		fmt.Fprintf(errOut, "ask: %s\n", sources)
	}
	if text == "" {
		err := errors.New("ask: claude: the answer was only its sources")
		fmt.Fprintf(errOut, "warning: %v\n", err)
		return askFailed(err), err
	}
	return text, nil
}

// sourcesLine finds the "Sources:" list that the web search tool makes Claude
// add after an answer, despite the system prompt: "Sources:", "## Sources",
// "**出典**：" and the like at the start of a line.
var sourcesLine = regexp.MustCompile(`(?m)^[ \t]*(?:#+[ \t]*)?(?:\*\*)?(?:Sources?|出典)(?:\*\*)?[ \t]*(?:[:：]|$)`)

// splitSources cuts the sources list off the answer, so its links are not
// read aloud, and returns it on one line for the log. The list must hold a
// link, so a "出典：古事記" inside the answer is kept. A separator line
// ("---") before the list goes with it. text is "" when the answer was only
// the list.
func splitSources(answer string) (text, sources string) {
	answer = strings.TrimSpace(answer)
	loc := sourcesLine.FindStringIndex(answer)
	if loc == nil || !strings.Contains(answer[loc[0]:], "http") {
		return answer, ""
	}
	return strings.TrimRight(answer[:loc[0]], " \t\r\n-*_"), strings.Join(strings.Fields(answer[loc[0]:]), " ")
}

// cmdAsk is `ask [--quiet] <question>`: Claude's answer is printed and, unless
// --quiet, spoken with the thinking face while it waits. Claude starts at
// once, alongside a one-shot gateway's start and the face change.
func cmdAsk(args []string, e cmdEnv) error {
	set, verbose := fs("ask")
	quiet := set.Bool("quiet", false, "print the answer only (no device)")
	pos, err := parseInterspersed(set, args)
	if err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(pos, " "))
	if question == "" {
		return errors.New("ask requires a question")
	}
	c, err := loadAskConfig()
	if err != nil {
		fmt.Fprintf(e.errOut, "warning: %v (using the defaults)\n", err)
	}
	type answer struct {
		text, notes string
		err         error
	}
	// Its notes are written by this command's goroutine, not alongside it.
	answered := make(chan answer, 1)
	go func() {
		var notes strings.Builder
		text, err := askAndReport(question, c, &notes)
		answered <- answer{text, notes.String(), err}
	}()
	if *quiet {
		a := <-answered
		fmt.Fprint(e.errOut, a.notes)
		fmt.Fprintln(e.out, a.text)
		return a.err
	}
	shown := false
	show := func() answer {
		a := <-answered
		fmt.Fprint(e.errOut, a.notes)
		fmt.Fprintln(e.out, a.text)
		shown = true
		return a
	}
	err = withDevice(*verbose, e.c, func(cl *mcp.Client) error {
		setFace(cl, "thinking", e.errOut)
		a := show()
		e.c = cl
		// "--": an answer may start with "-" (-5度…), which is not a flag.
		if err := cmdSay([]string{"--while-face", restFace, "--", a.text}, e); err != nil {
			return err
		}
		return a.err
	})
	if !shown {
		// The device never came: the answer (already paid for) is still shown.
		show()
	}
	return err
}
