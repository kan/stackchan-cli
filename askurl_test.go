package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExtractURLs(t *testing.T) {
	tests := []struct {
		in, spoken string
		urls       []string
	}{
		{"晴れだよ。", "晴れだよ。", nil},
		{"公式ページに載ってるよ。\nhttps://example.com/a?b=1&c=2", "公式ページに載ってるよ。", []string{"https://example.com/a?b=1&c=2"}},
		{"詳しくは[気象庁](https://www.jma.go.jp/)を見てね。", "詳しくは気象庁を見てね。", []string{"https://www.jma.go.jp/"}},
		{"ここだよ（https://example.com/x）。", "ここだよ。", []string{"https://example.com/x"}},
		{"ここだよ(https://example.com/x)。", "ここだよ。", []string{"https://example.com/x"}},
		{"https://ja.wikipedia.org/wiki/Foo_(bar) を見てね", "を見てね", []string{"https://ja.wikipedia.org/wiki/Foo_(bar)"}},
		{"[ページ](https://ja.wikipedia.org/wiki/Foo_(bar))だよ", "ページだよ", []string{"https://ja.wikipedia.org/wiki/Foo_(bar)"}},
		{"https://www.jma.go.jp/を見てね", "を見てね", []string{"https://www.jma.go.jp/"}},
		{"https://ja.wikipedia.org/wiki/東京タワー", "", []string{"https://ja.wikipedia.org/wiki/東京タワー"}},
		{"手順は次のとおり：\n見てね。\nURL: <https://example.com/>", "手順は次のとおり：\n見てね。", []string{"https://example.com/"}},
		{"詳しくはこちら：https://a.example/ と https://a.example/", "詳しくはこちら： と", []string{"https://a.example/"}},
		{"https://example.com/.", "", []string{"https://example.com/"}},
		{"ftp://example.com/ は開かない", "ftp://example.com/ は開かない", nil},
		{"https:// だけ", "https:// だけ", nil},
	}
	for _, tt := range tests {
		spoken, urls := extractURLs(tt.in)
		if spoken != tt.spoken || !slices.Equal(urls, tt.urls) {
			t.Errorf("extractURLs(%q) = %q, %q; want %q, %q", tt.in, spoken, urls, tt.spoken, tt.urls)
		}
	}
}

func TestValidWebURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://example.com/":      true,
		"http://example.com/a?b=&c": true,
		"file:///C:/Windows":        false,
		"javascript:alert(1)":       false,
		"https:///nohost":           false,
		"example.com":               false,
	} {
		if got := validWebURL(s); got != want {
			t.Errorf("validWebURL(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestURLOffer(t *testing.T) {
	var o urlOffer
	now := time.Now()
	if _, ok := o.take(now); ok {
		t.Fatal("took an offer that was never made")
	}
	o.offer("https://a.example/", now)
	if _, ok := o.take(now.Add(-time.Second)); ok {
		t.Fatal("a touch made before the question answered it")
	}
	if u, ok := o.take(now.Add(time.Second)); !ok || u != "https://a.example/" {
		t.Fatalf("take = %q, %v", u, ok)
	}
	if _, ok := o.take(now.Add(2 * time.Second)); ok {
		t.Fatal("an offer was taken twice")
	}
	o.offer("https://b.example/", now)
	if _, ok := o.take(now.Add(urlWait + time.Second)); ok {
		t.Fatal("took an offer after it expired")
	}
	o.offer("https://c.example/", now)
	if old := o.offer("https://d.example/", now); old != "https://c.example/" {
		t.Errorf("offer replaced %q, want c", old)
	}
	if u := o.cancel(now); u != "https://d.example/" {
		t.Errorf("cancel = %q, want d", u)
	}
	if _, ok := o.take(now); ok {
		t.Fatal("took an offer after cancel")
	}
	var none *urlOffer // no daemon
	if _, ok := none.take(now); ok || none.cancel(now) != "" {
		t.Fatal("a nil offer had something to take")
	}
}

// fakeBrowser records the URLs opened (and silences the robot's reply), and
// gives the daemon an offer.
func fakeBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	oldOpen, oldSay := openBrowser, sayReply
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	sayReply = func(string, cmdEnv) {}
	daemonURLOffer = &urlOffer{}
	t.Cleanup(func() { openBrowser, sayReply, daemonURLOffer = oldOpen, oldSay, nil })
	return &opened
}

func TestAnswerURLByVoice(t *testing.T) {
	openedp := fakeBrowser(t)
	var log strings.Builder
	e := cmdEnv{out: &log, errOut: &log}

	if answerURLByVoice("はい", e) {
		t.Fatal("a yes with nothing asked about was taken")
	}
	daemonURLOffer.offer("https://a.example/", time.Now().Add(-time.Second))
	if answerURLByVoice("今何時", e) {
		t.Fatal("other speech was taken as the answer")
	}
	if !answerURLByVoice("はい", e) || !slices.Equal(*openedp, []string{"https://a.example/"}) {
		t.Fatalf("a yes did not open the page: %q", *openedp)
	}
	daemonURLOffer.offer("https://c.example/", time.Now().Add(-time.Second))
	if !answerURLByVoice("開いてください", e) || len(*openedp) != 2 {
		t.Fatalf("「開いてください」 did not open the page: %q", *openedp)
	}
	if isAnswer("うん") {
		t.Error("「うん」 became a pomodoro answer too")
	}
	daemonURLOffer.offer("https://b.example/", time.Now().Add(-time.Second))
	if !answerURLByVoice("いいえです", e) || len(*openedp) != 2 {
		t.Fatalf("a no did not end the offer without opening: %q", *openedp)
	}
	if _, ok := daemonURLOffer.take(time.Now()); ok {
		t.Fatal("the offer still waited after a no")
	}
}

func TestIsDecline(t *testing.T) {
	for heard, want := range map[string]bool{
		"いいえ。":         true,
		"開かないで":        true,
		"スタックチャン、いらない": true,
		"いらないです":       true,
		"いいえ、明日の天気は":   false,
		"はい":           false,
	} {
		if got := isDecline(heard); got != want {
			t.Errorf("isDecline(%q) = %v, want %v", heard, got, want)
		}
	}
}

func TestCmdAskOpen(t *testing.T) {
	openedp := fakeBrowser(t)
	var out strings.Builder
	e := cmdEnv{out: &out, errOut: &out}

	daemonURLOffer = nil
	if err := cmdAsk([]string{"--open"}, e); err == nil {
		t.Fatal("ask --open without the daemon did not fail")
	}
	daemonURLOffer = &urlOffer{}
	if err := cmdAsk([]string{"--open"}, e); err != nil || !strings.Contains(out.String(), "no page") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	daemonURLOffer.offer("https://a.example/", time.Now().Add(-time.Second))
	if err := cmdAsk([]string{"--open", "foo"}, e); err == nil {
		t.Fatal("ask --open with a question did not fail")
	}
	if err := cmdAsk([]string{"--open"}, e); err != nil || !slices.Equal(*openedp, []string{"https://a.example/"}) {
		t.Fatalf("err=%v opened=%q", err, *openedp)
	}
}
