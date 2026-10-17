package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeSpeech(t *testing.T) {
	tests := []struct{ in, want string }{
		{"スタックチャン、笑って！", "すたっくちゃん笑って"},
		{"わらって。", "わらって"},
		{"ＬＥＤ　つけて", "ledつけて"},
		{"LED つけて?", "ledつけて"},
		{"ヴァイオリン", "ゔぁいおりん"},
		{"ラーメン", "らーめん"}, // the long-vowel mark stays
		{"「写真」撮って…", "写真撮って"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeSpeech(tt.in); got != tt.want {
			t.Errorf("normalizeSpeech(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitCommands(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{" avatar happy ", []string{"avatar happy"}},
		{"move-head --yaw 30; sleep 500;move-head --yaw 0", []string{"move-head --yaw 30", "sleep 500", "move-head --yaw 0"}},
		{`say "a; b"; avatar idle`, []string{`say "a; b"`, "avatar idle"}},
		{`say 'it;s'`, []string{`say 'it;s'`}},
		{" ; ", nil},
	}
	for _, tt := range tests {
		if got := splitCommands(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCommands(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVoiceRulesMatch(t *testing.T) {
	vr, err := parseVoiceRules(`
# comment
笑って|ワラッテ => avatar happy
写真 => photo; say "撮ったよ"
笑 => avatar sad
* => avatar thinking
`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		heard   string
		want    []string
		keyword string
	}{
		{"スタックチャン、笑ってください。", []string{"avatar happy"}, "笑って"},
		{"わらってー", []string{"avatar happy"}, "わらって"}, // katakana keyword, hiragana speech
		{"写真を撮って", []string{"photo", `say "撮ったよ"`}, "写真"},
		{"笑", []string{"avatar sad"}, "笑"},          // the earlier rule needs "笑って"
		{"写真で笑って", []string{"avatar happy"}, "笑って"}, // the earlier line wins
		{"こんにちは", []string{"avatar thinking"}, ""},
		{"", []string{"avatar thinking"}, ""}, // nothing heard
	}
	for _, tt := range tests {
		got, keyword := vr.match(tt.heard)
		if !reflect.DeepEqual(got, tt.want) || keyword != tt.keyword {
			t.Errorf("match(%q) = %q (keyword %q), want %q (keyword %q)", tt.heard, got, keyword, tt.want, tt.keyword)
		}
	}
}

func TestVoiceRulesWithoutFallback(t *testing.T) {
	vr, err := parseVoiceRules("笑って => avatar happy\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, keyword := vr.match("こんにちは"); got != nil || keyword != "" {
		t.Errorf("match = %q, %q; want nothing", got, keyword)
	}
}

func TestParseVoiceRulesErrors(t *testing.T) {
	for _, text := range []string{
		"笑って avatar happy",   // no =>
		"笑って =>",             // no command
		"笑って => ;",           // no command
		"、。 => avatar happy", // keyword is only punctuation
		"| => avatar happy",
	} {
		if _, err := parseVoiceRules(text); err == nil {
			t.Errorf("parseVoiceRules(%q) accepted it", text)
		}
	}
}

func TestDefaultVoiceCommandsParse(t *testing.T) {
	vr, err := parseVoiceRules(defaultVoiceCommands)
	if err != nil {
		t.Fatal(err)
	}
	if len(vr.rules) == 0 || vr.fallback == nil {
		t.Errorf("defaults have %d rules, fallback %v", len(vr.rules), vr.fallback)
	}
	if got, _ := vr.match("スタックチャン、写真撮って"); !reflect.DeepEqual(got, []string{`say --face happy "撮るよ"`, "photo", `say --face happy "撮ったよ"`}) {
		t.Errorf("写真撮って -> %q", got)
	}
	if got, _ := vr.match("いま何時？"); !reflect.DeepEqual(got, []string{`say "{date}、{weekday}、{time}です"`}) {
		t.Errorf("いま何時？ -> %q", got)
	}
}

// runVoice reads the user's rules file and runs the matched commands.
func TestRunVoiceUsesRulesFile(t *testing.T) {
	fakeHome(t)
	seedFile(voiceCommandsPath(), "ヘルプ => help\n")
	var out, errOut bytes.Buffer
	if err := runVoice("ヘルプを見せて", cmdEnv{out: &out, errOut: &errOut}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "commands (device stays connected") {
		t.Errorf("out = %q, want the help text", out.String())
	}
	if !strings.Contains(errOut.String(), `-> "へるぷ": help`) {
		t.Errorf("errOut = %q, want the match report", errOut.String())
	}

	// A failing command is reported as an error.
	if err := os.WriteFile(voiceCommandsPath(), []byte("ヘルプ => sleep x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runVoice("ヘルプ", cmdEnv{out: &out, errOut: &errOut}); err == nil {
		t.Error("runVoice with a failing command returned nil")
	}
}

func TestExpandSayVars(t *testing.T) {
	sat := time.Date(2026, 10, 17, 22, 5, 0, 0, time.Local)
	tests := []struct{ in, want string }{
		{"{date}、{weekday}、{time}です", "2026年10月17日、土曜日、22時5分です"},
		{"今日は{weekday}", "今日は土曜日"},
		{"{unknown} と {", "{unknown} と {"},
		{"こんにちは", "こんにちは"},
	}
	for _, tt := range tests {
		if got := expandSayVars(tt.in, sat); got != tt.want {
			t.Errorf("expandSayVars(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := expandSayVars("{weekday}", sat.AddDate(0, 0, 1)); got != "日曜日" {
		t.Errorf("Sunday = %q", got)
	}
}

// A rule cannot call voice again: it could recurse until the daemon crashes.
func TestVoiceRuleCannotCallVoice(t *testing.T) {
	fakeHome(t)
	seedFile(voiceCommandsPath(), "ループ => voice ループ\n")
	var buf bytes.Buffer
	err := runVoice("ループ", cmdEnv{out: &buf, errOut: &buf})
	if err == nil || !strings.Contains(buf.String(), "voice cannot be used inside a voice command rule") {
		t.Errorf("err = %v, output %q; want the nested voice refused", err, buf.String())
	}
}

// A failed transcription runs the * rule (as for nothing heard), so the
// failure shows on the device.
func TestVoiceHookRunsFallbackOnTranscribeError(t *testing.T) {
	heard := make(chan string, 1)
	h := &voiceHook{
		transcribe: func([]byte) (string, error) { return "", errors.New("no python") },
		run:        func(text string) { heard <- text },
		face:       func(string) {},
		jobs:       make(chan []byte, 1),
	}
	stop := make(chan struct{})
	defer close(stop)
	go h.process(stop)
	h.jobs <- []byte("OggS")
	select {
	case text := <-heard:
		if text != "" {
			t.Errorf("run(%q), want run(\"\")", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was run after the transcription failed")
	}
}

func TestVoiceHookRejectsHugeAudio(t *testing.T) {
	h := &voiceHook{jobs: make(chan []byte, 1)}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/voice", "audio/ogg", bytes.NewReader(make([]byte, 16<<20+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || len(h.jobs) != 0 {
		t.Errorf("status = %d, queued %d; want 413 and nothing queued", resp.StatusCode, len(h.jobs))
	}
}

// `voice` without text lists the rules: the defaults until the daemon has
// written the file, then the file's rule lines.
func TestListVoiceRules(t *testing.T) {
	fakeHome(t)
	var buf bytes.Buffer
	if err := listVoiceRules(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "# defaults") || !strings.Contains(buf.String(), "笑って|わらって|にこにこ => avatar happy") {
		t.Errorf("list without a file = %q, want the defaults", buf.String())
	}
	if strings.Contains(buf.String(), "書式") {
		t.Errorf("comments were listed: %q", buf.String())
	}

	seedFile(voiceCommandsPath(), "# note\nヘルプ => help\n")
	buf.Reset()
	if err := listVoiceRules(&buf); err != nil {
		t.Fatal(err)
	}
	if want := "# " + voiceCommandsPath() + "\nヘルプ => help\n"; buf.String() != want {
		t.Errorf("list = %q, want %q", buf.String(), want)
	}
}

func TestVoiceHookTranscribesAndRuns(t *testing.T) {
	heard := make(chan string, 1)
	var gotAudio []byte
	var mu sync.Mutex
	var steps []string // faces and the transcription, in order
	step := func(s string) { mu.Lock(); steps = append(steps, s); mu.Unlock() }
	thinkingShown := make(chan struct{})
	h := &voiceHook{
		transcribe: func(audio []byte) (string, error) {
			<-thinkingShown // make the order deterministic for the check below
			gotAudio = audio
			step("transcribe")
			return "笑って", nil
		},
		run: func(text string) { step("run"); heard <- text },
		face: func(name string) {
			step(name)
			if name == "thinking" {
				close(thinkingShown)
			}
		},
		jobs: make(chan []byte, 1),
	}
	stop := make(chan struct{})
	defer close(stop)
	go h.process(stop)

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/voice", "audio/ogg", strings.NewReader("OggS..."))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	select {
	case text := <-heard:
		if text != "笑って" || string(gotAudio) != "OggS..." {
			t.Errorf("run(%q) with audio %q", text, gotAudio)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription was never run")
	}
	mu.Lock()
	if got, want := strings.Join(steps, ","), "thinking,transcribe,"+restFace+",run"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	mu.Unlock()

	resp, err = http.Get(srv.URL + "/voice")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}
