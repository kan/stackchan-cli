package main

// Voice commands. The device starts listening on a screen tap and sends the
// audio on the next tap; the gateway POSTs it (Ogg/Opus) to
// STACKCHAN_AUDIO_HOOK_URL. The daemon serves that URL, transcribes the audio
// with the gateway's own faster-whisper, and runs the first rule in
// ~/.stackchan/voice-commands.txt whose keyword the text contains.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"stackchan-cli/internal/mcp"
)

const (
	// voiceHookAddr is where the daemon receives the gateway's audio POSTs.
	voiceHookAddr     = "127.0.0.1:8771"
	transcribeTimeout = 60 * time.Second // a few seconds normally
)

func voiceCommandsPath() string { return filepath.Join(stackchanDir(), "voice-commands.txt") }

// defaultVoiceCommands seeds voice-commands.txt and is used while it is missing.
const defaultVoiceCommands = `# 音声コマンド: 画面をタップ → 話す → もう一度タップ で聞き取り、
# 聞き取った文にキーワードが含まれていたら、=> の右のコマンドを実行する（daemon が必要）。
#
# 書式: キーワード|別の書き方 => コマンド; コマンド; ...
# - 上の行ほど優先。* の行はどれにも当たらなかったとき（聞き取れなかったときも）。
# - 照合の前に、全角カタカナはひらがなに、全角英数は半角に、大文字は小文字にそろえ、
#   記号と空白は取り除く（キーワードも同じようにそろえる）。
# - コマンドは REPL と同じ（avatar / say / move-head / photo / sleep / all-leds ...）。
# - 保存すれば次の聞き取りから反映される（再起動は要らない）。
# - stackchan-cli voice "笑って" で、話さずに試せる。stackchan-cli voice だけなら一覧を表示する。
笑って|わらって|にこにこ => avatar happy
悲しい|かなしい => avatar sad
写真|しゃしん|撮って => say --face happy "撮るよ"; photo; say --face happy "撮ったよ"
首振って|首ふって|くびふって => move-head --yaw -30 --pitch 45; sleep 500; move-head --yaw 30 --pitch 45; sleep 500; move-head --yaw 0 --pitch 45
* => say --face sad "ごめんね、わからなかった"; sleep 2000; avatar idle
`

type voiceRule struct {
	keywords []string // normalized
	commands []string // REPL lines
}

type voiceRules struct {
	rules    []voiceRule
	fallback []string // the "*" rule; nil when there is none
}

// parseVoiceRules reads the voice-commands.txt format (see defaultVoiceCommands).
func parseVoiceRules(text string) (voiceRules, error) {
	var vr voiceRules
	i := 0
	for line := range strings.SplitSeq(text, "\n") {
		i++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lhs, rhs, ok := strings.Cut(line, "=>")
		if !ok {
			return vr, fmt.Errorf("line %d: missing =>", i)
		}
		commands := splitCommands(rhs)
		if len(commands) == 0 {
			return vr, fmt.Errorf("line %d: no command after =>", i)
		}
		if strings.TrimSpace(lhs) == "*" {
			vr.fallback = commands
			continue
		}
		var keywords []string
		for k := range strings.SplitSeq(lhs, "|") {
			if n := normalizeSpeech(k); n != "" {
				keywords = append(keywords, n)
			}
		}
		if len(keywords) == 0 {
			return vr, fmt.Errorf("line %d: no keyword before =>", i)
		}
		vr.rules = append(vr.rules, voiceRule{keywords: keywords, commands: commands})
	}
	return vr, nil
}

// match returns the commands for heard: the first rule with a keyword that
// heard contains, else the fallback. keyword is "" for the fallback.
func (vr voiceRules) match(heard string) (commands []string, keyword string) {
	if h := normalizeSpeech(heard); h != "" {
		for _, r := range vr.rules {
			for _, k := range r.keywords {
				if strings.Contains(h, k) {
					return r.commands, k
				}
			}
		}
	}
	return vr.fallback, ""
}

// normalizeSpeech folds the differences speech recognition varies on:
// full-width katakana to hiragana (half-width katakana is left as is), full-width ASCII to ASCII, case, and drops
// punctuation, symbols and spaces. The long-vowel mark "ー" is kept.
func normalizeSpeech(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'ァ' && r <= 'ヶ':
			r -= 'ァ' - 'ぁ'
		case r >= '！' && r <= '～':
			r -= '！' - '!'
		case r == '　':
			r = ' '
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// splitCommands splits "cmd; cmd" on semicolons outside quotes.
func splitCommands(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if c := strings.TrimSpace(cur.String()); c != "" {
			out = append(out, c)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ';':
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// readVoiceRules returns the text of voice-commands.txt, or the defaults
// (exists=false) while it is missing.
func readVoiceRules() (text string, exists bool, err error) {
	data, err := os.ReadFile(voiceCommandsPath())
	if errors.Is(err, os.ErrNotExist) {
		return defaultVoiceCommands, false, nil
	}
	return string(data), err == nil, err
}

// loadVoiceRules parses voice-commands.txt, or the defaults while it is missing.
func loadVoiceRules() (voiceRules, error) {
	text, _, err := readVoiceRules()
	if err != nil {
		return voiceRules{}, err
	}
	vr, err := parseVoiceRules(text)
	if err != nil {
		return vr, fmt.Errorf("%s: %w", voiceCommandsPath(), err)
	}
	return vr, nil
}

// listVoiceRules prints the rule lines (comments left out) and where they come from.
func listVoiceRules(w io.Writer) error {
	text, exists, err := readVoiceRules()
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(w, "# %s\n", voiceCommandsPath())
	} else {
		fmt.Fprintf(w, "# defaults (%s is created when the daemon starts)\n", voiceCommandsPath())
	}
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			fmt.Fprintln(w, line)
		}
	}
	return nil
}

// runVoice matches heard against the rules and runs what it matched,
// reporting the match to e.errOut.
func runVoice(heard string, e cmdEnv) error {
	vr, err := loadVoiceRules()
	if err != nil {
		return err
	}
	commands, keyword := vr.match(heard)
	switch {
	case keyword != "":
		fmt.Fprintf(e.errOut, "voice: heard %q -> %q: %s\n", heard, keyword, strings.Join(commands, "; "))
	case commands != nil:
		fmt.Fprintf(e.errOut, "voice: heard %q -> no keyword; running the * rule\n", heard)
	default:
		fmt.Fprintf(e.errOut, "voice: heard %q -> no keyword (no * rule)\n", heard)
		return nil
	}
	e.inVoice = true
	if failed := runScript(strings.Join(commands, "\n"), e); failed > 0 {
		return fmt.Errorf("voice: %d command(s) failed", failed)
	}
	return nil
}

// cmdVoice runs the voice rules on given text, to try them without speaking;
// without text it lists the rules.
func cmdVoice(args []string, e cmdEnv) error {
	set, _ := fs("voice")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return listVoiceRules(e.out)
	}
	if e.inVoice {
		return errors.New("voice cannot be used inside a voice command rule")
	}
	return runVoice(strings.Join(set.Args(), " "), e)
}

// ---- daemon side ----------------------------------------------------------

// transcribeScript runs in the gateway's Python, which has faster-whisper and
// the model already, with the settings of the gateway's listen.
const transcribeScript = `import os, sys
from faster_whisper import WhisperModel
m = WhisperModel(sys.argv[2], device=os.getenv("STACKCHAN_FASTER_WHISPER_DEVICE") or "cpu",
                 compute_type=os.getenv("STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE") or "int8")
segments, _ = m.transcribe(sys.argv[1], language="ja", beam_size=1, vad_filter=False)
sys.stdout.write("".join(s.text for s in segments).strip())
`

// sttPython finds the Python of the uv-installed gateway.
func sttPython() string {
	if p := os.Getenv("STACKCHAN_STT_PYTHON"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(os.Getenv("APPDATA"), "uv", "tools", "stackchan-mcp", "Scripts", "python.exe"),
		filepath.Join(home, ".local", "share", "uv", "tools", "stackchan-mcp", "bin", "python"),
	} {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// transcribeAudio turns Ogg/Opus audio into text with faster-whisper.
func transcribeAudio(audio []byte) (string, error) {
	py := sttPython()
	if py == "" {
		return "", errors.New("the gateway's Python was not found (set STACKCHAN_STT_PYTHON)")
	}
	f, err := os.CreateTemp(stackchanDir(), "voice-*.ogg")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(audio)
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", err
	}
	model := os.Getenv("STACKCHAN_FASTER_WHISPER_MODEL")
	if model == "" {
		model = defaultWhisperModel
	}
	// A stuck transcriber would block every later voice command.
	ctx, cancel := context.WithTimeout(context.Background(), transcribeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-X", "utf8", "-c", transcribeScript, f.Name(), model)
	// python.exe of a uv venv may run the interpreter as its own child, which
	// a timeout does not kill; stop waiting for its pipes shortly after.
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("transcribe: no result within %s (a model that is not downloaded yet is fetched on first use; run `stackchan-cli listen` once to fetch %s)", transcribeTimeout, model)
	}
	if err != nil {
		return "", fmt.Errorf("transcribe: %v: %s", err, lastLine(stderr.String()))
	}
	return string(out), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// voiceHook receives the gateway's audio POSTs. One utterance is handled at
// a time; one more waits, and further ones are dropped (the device cannot
// listen while we transcribe, so this only happens when transcribing stalls).
type voiceHook struct {
	transcribe func(audio []byte) (string, error)
	run        func(heard string) // runVoice in the daemon
	face       func(name string)  // set_avatar; shows thinking while transcribing
	jobs       chan []byte
}

func newVoiceHook(c *mcp.Client) *voiceHook {
	e := cmdEnv{c: c, out: os.Stderr, errOut: os.Stderr}
	return &voiceHook{
		transcribe: transcribeAudio,
		run: func(heard string) {
			if err := runVoice(heard, e); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			}
		},
		face: func(name string) {
			if _, err := callTool(c, "set_avatar", map[string]any{"face": name}, nil); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: voice: set_avatar %s: %v\n", name, err)
			}
		},
		jobs: make(chan []byte, 1),
	}
}

// ServeHTTP answers at once (the gateway waits at most 10s) and leaves the
// work to process.
func (h *voiceHook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	const maxAudio = 16 << 20 // minutes of speech at the device's bitrate
	audio, err := io.ReadAll(io.LimitReader(r.Body, maxAudio+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(audio) > maxAudio {
		fmt.Fprintln(os.Stderr, "daemon: voice dropped (audio over 16MB)")
		http.Error(w, "audio too large", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case h.jobs <- audio:
	default:
		fmt.Fprintln(os.Stderr, "daemon: voice dropped (still transcribing the previous one)")
	}
	w.WriteHeader(http.StatusNoContent)
}

// process transcribes and runs queued utterances until stop is closed.
func (h *voiceHook) process(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case audio := <-h.jobs:
			fmt.Fprintf(os.Stderr, "daemon: voice received (%d bytes), transcribing...\n", len(audio))
			// The thinking face goes out alongside the transcription, so a slow
			// device does not delay it.
			thinking := make(chan struct{})
			go func() { defer close(thinking); h.face("thinking") }()
			heard, err := h.transcribe(audio)
			<-thinking
			// Back to idle before the commands, so a rule that sets no face
			// does not leave the thinking one.
			h.face("idle")
			if err != nil {
				// Run the * rule as for nothing heard, so the failure shows on
				// the device too.
				fmt.Fprintf(os.Stderr, "daemon: voice: %v\n", err)
				heard = ""
			}
			h.run(heard)
		}
	}
}

// serveVoice serves the hook on ln until it is closed.
func serveVoice(ln net.Listener, h *voiceHook, stop <-chan struct{}) {
	go h.process(stop)
	_ = http.Serve(ln, h)
}
