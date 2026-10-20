package main

// Voice commands. The device starts listening on a screen tap and sends the
// audio on the next tap; the gateway POSTs it (Ogg/Opus) to
// STACKCHAN_AUDIO_HOOK_URL. The daemon serves that URL, transcribes the audio
// with the gateway's own faster-whisper, and runs the first rule in
// ~/.stackchan/voice-commands.txt whose keyword the text contains.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
#   say の文の {date} {weekday} {time} は、その時の日付・曜日・時刻（例: 2026年10月17日、土曜日、22時40分）になる。
#   {weather} は今日の天気・気温・降水確率の文、{weather_sky} は天気だけ、{weather_temp} は気温だけ、
#   {weather_rain} は降水確率だけ。末尾に _tomorrow を付けると明日の分（例: {weather_temp_tomorrow}）。
#   地域は ~/.stackchan/weather.txt に書く。
#   {pc} はこの PC の調子（CPU・メモリ・ディスクの空き。気になる値から先に言い、話す間の表情も変わる）。
#   {cpu} {mem} は使用率の数字だけ（例: 12）、{disk} はディスクの空き（例: 120ギガバイト）。
#   閾値やドライブは ~/.stackchan/pc-monitor.txt に書く。
# - 保存すれば次の聞き取りから反映される（再起動は要らない）。
# - stackchan-cli voice "笑って" で、話さずに試せる。stackchan-cli voice だけなら一覧を表示する。
笑って|わらって|にこにこ => avatar happy
悲しい|かなしい => avatar sad
写真|しゃしん|撮って => say --face happy "撮るよ"; photo; say --face happy "撮ったよ"
首振って|首ふって|くびふって => move-head --yaw -30 --pitch 45; sleep 500; move-head --yaw 30 --pitch 45; sleep 500; move-head --yaw 0 --pitch 45
明日の気温|あしたの気温|明日の温度|あしたの温度|明日何度|明日は何度|あしたなんど => say "{weather_temp_tomorrow}"
明日の雨|あしたの雨|明日は雨|あしたは雨|明日雨|明日の降水確率|あしたの降水確率|明日傘|明日は傘|明日の傘|明日かさ|あしたかさ => say "{weather_rain_tomorrow}"
明日|あした => say "{weather_tomorrow}"
天気だけ|てんきだけ => say "{weather_sky}"
気温|温度|何度|なんど|きおん => say "{weather_temp}"
雨|降水確率|傘|かさ => say "{weather_rain}"
天気|てんき => say "{weather}"
調子どう|ちょうしどう|調子は|ちょうしは|元気|げんき => say "{pc}"
何時|なんじ|何日|なんにち|日付 => say "{date}、{weekday}、{time}です"
* => say --face sad "ごめんね、わからなかった"; sleep 2000; avatar embarrassed
`

type voiceRule struct {
	keywords []string // normalized
	commands []string // REPL lines
}

type voiceRules struct {
	rules    []voiceRule
	fallback []string // the "*" rule; nil when there is none
	words    []string // keywords for the recognizer's prompt, each once (see sttPrompt)
}

// sttPromptMaxRunes keeps the prompt well inside what whisper uses: it keeps
// only the last ~223 tokens of a prompt, and a kanji can take 2 or more, so
// a longer prompt would silently lose its start.
const sttPromptMaxRunes = 120

// sttPrompt is the text given to the speech recognizer as its initial
// prompt: the name StackChan is called by and the rules' keywords. A short
// utterance has little context, and whisper then drifts to common phrases
// (気温は → キヨンは, → お疲れ様); the prompt steers it to the words the rules
// listen for. Keywords past sttPromptMaxRunes are left out. "" when there
// are no rules.
func (vr voiceRules) sttPrompt() string {
	if len(vr.words) == 0 {
		return ""
	}
	p := "スタックチャン"
	for _, w := range vr.words {
		// A compound (明日の気温, 天気だけ) adds nothing when its parts are there.
		if slices.ContainsFunc(vr.words, func(o string) bool { return o != w && strings.Contains(w, o) }) {
			continue
		}
		if utf8.RuneCountInString(p)+1+utf8.RuneCountInString(w) > sttPromptMaxRunes-1 {
			break
		}
		p += "、" + w
	}
	return p + "。"
}

// promptWords picks a rule's keywords for the prompt: the ones written with
// kanji (天気, not its reading てんき, which only repeats it), or the first
// keyword when all are kana.
func promptWords(keywords []string) []string {
	var words []string
	for _, k := range keywords {
		if strings.IndexFunc(k, func(r rune) bool { return !isKana(r) }) >= 0 {
			words = append(words, k)
		}
	}
	if len(words) == 0 && len(keywords) > 0 {
		words = keywords[:1]
	}
	return words
}

func isKana(r rune) bool { return r >= 'ぁ' && r <= 'ゖ' || r >= 'ァ' && r <= 'ヺ' || r == 'ー' }

// isPromptEcho reports whether heard is only a piece of the prompt listing
// several of its words: whisper sometimes returns its prompt for silence or
// a mumble, and the keywords in it would then run a rule nobody asked for.
// A real request names one or two ("スタックチャン、笑って"), so it takes
// three words besides the name.
func isPromptEcho(heard, prompt string) bool {
	h := normalizeSpeech(heard)
	if h == "" || !strings.Contains(normalizeSpeech(prompt), h) {
		return false
	}
	words := strings.FieldsFunc(prompt, func(r rune) bool { return r == '、' || r == '。' })
	n := 0
	for _, w := range words[min(1, len(words)):] { // words[0] is the name
		if strings.Contains(h, normalizeSpeech(w)) {
			n++
		}
	}
	return n >= 3
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
		var keywords, written []string
		for k := range strings.SplitSeq(lhs, "|") {
			if n := normalizeSpeech(k); n != "" {
				keywords = append(keywords, n)
				written = append(written, strings.TrimSpace(k))
			}
		}
		if len(keywords) == 0 {
			return vr, fmt.Errorf("line %d: no keyword before =>", i)
		}
		for _, w := range promptWords(written) {
			if !slices.Contains(vr.words, w) {
				vr.words = append(vr.words, w)
			}
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

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// voiceHook receives the gateway's audio POSTs. One utterance is handled at
// a time; one more waits, and further ones are dropped (the device cannot
// listen while we transcribe, so this only happens when transcribing stalls).
type voiceHook struct {
	transcribe func(audio []byte, prompt string) (string, error)
	prompt     func() string                    // voiceSTTPrompt; nil gives none
	run        func(heard string)               // runVoice in the daemon
	face       func(name string)                // set_avatar; shows thinking while transcribing
	keep       func(audio []byte, heard string) // saveVoiceSample; nil keeps nothing
	jobs       chan []byte
}

// newVoiceHook transcribes with a resident worker, started in the background
// right away so the first utterance does not wait for the model.
func newVoiceHook(c *mcp.Client) *voiceHook {
	e := cmdEnv{c: c, out: os.Stderr, errOut: os.Stderr}
	stt := newSTTWorker()
	go stt.warm()
	var keep func([]byte, string)
	if os.Getenv("STACKCHAN_VOICE_SAMPLES") != "0" {
		keep = saveVoiceSample
	}
	return &voiceHook{
		transcribe: stt.transcribe,
		prompt:     voiceSTTPrompt,
		keep:       keep,
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
			prompt := ""
			if h.prompt != nil {
				prompt = h.prompt()
			}
			heard, err := h.transcribe(audio, prompt)
			<-thinking
			// Back to the rest face before the commands, so a rule that sets no face
			// does not leave the thinking one.
			h.face(restFace)
			saved := heard
			switch {
			case err != nil:
				// Run the * rule as for nothing heard, so the failure shows on
				// the device too.
				fmt.Fprintf(os.Stderr, "daemon: voice: %v\n", err)
				heard, saved = "", "(error: "+err.Error()+")"
			case isPromptEcho(heard, prompt):
				fmt.Fprintf(os.Stderr, "daemon: voice: heard %q is the recognizer's prompt echoed; treating it as nothing heard\n", heard)
				heard, saved = "", "(prompt echoed: "+heard+")"
			}
			if h.keep != nil {
				go h.keep(audio, saved) // diagnostics only: do not delay the reply
			}
			h.run(heard)
		}
	}
}

// voiceSTTPrompt is the recognizer's prompt from the current rules, or ""
// with STACKCHAN_STT_PROMPT=0 (or no rules).
func voiceSTTPrompt() string {
	if os.Getenv("STACKCHAN_STT_PROMPT") == "0" {
		return ""
	}
	vr, err := loadVoiceRules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: voice: no recognizer prompt: %v\n", err)
		return ""
	}
	return vr.sttPrompt()
}

func voiceSamplesDir() string { return filepath.Join(stackchanDir(), "voice-samples") }

// voiceSamplesKept is how many recent utterances saveVoiceSample keeps.
const voiceSamplesKept = 30

// saveVoiceSample keeps the audio of an utterance (<time>.ogg) and what was
// heard in it (<time>.txt; an error or an echoed prompt is noted in
// parentheses) in ~/.stackchan/voice-samples, the most recent
// voiceSamplesKept, so recognition can be checked and tuned against the
// device's real microphone. STACKCHAN_VOICE_SAMPLES=0 turns it off.
// Failures are only logged.
func saveVoiceSample(audio []byte, heard string) {
	dir := voiceSamplesDir()
	base := filepath.Join(dir, time.Now().Format("20060102-150405.000"))
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = os.WriteFile(base+".ogg", audio, 0o644)
	}
	if err == nil {
		err = os.WriteFile(base+".txt", []byte(heard+"\n"), 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: voice: keep sample: %v\n", err)
		return
	}
	pruneVoiceSamples(dir, voiceSamplesKept)
}

// pruneVoiceSamples removes all but the newest keep samples (names sort by
// time), counting a sample by its name whether its .ogg, its .txt or both
// are left.
func pruneVoiceSamples(dir string, keep int) {
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	var bases []string
	for _, f := range files {
		if ext := filepath.Ext(f); ext == ".ogg" || ext == ".txt" {
			if b := strings.TrimSuffix(f, ext); !slices.Contains(bases, b) {
				bases = append(bases, b)
			}
		}
	}
	slices.Sort(bases)
	for _, b := range bases[:max(0, len(bases)-keep)] {
		_ = os.Remove(b + ".ogg")
		_ = os.Remove(b + ".txt")
	}
}

// serveVoice serves the hook on ln until it is closed.
func serveVoice(ln net.Listener, h *voiceHook, stop <-chan struct{}) {
	go h.process(stop)
	_ = http.Serve(ln, h)
}
