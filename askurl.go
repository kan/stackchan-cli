package main

// URLs in ask's answers (issue #9): they are not read aloud. In the daemon,
// the robot asks whether to open the first one, and opens it in the PC's
// browser on a yes (a head touch, a short spoken yes, or `ask --open`). It
// never opens one without being told to.

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"stackchan-cli/internal/mcp"
)

// askURLQuestion is said after an answer holding a URL.
const askURLQuestion = "パソコンでページを開く？"

// askURLOnly is said when the answer was only a URL.
const askURLOnly = "ページを見つけたよ。"

// urlWait is how long the offer waits for a yes.
const urlWait = time.Minute

var (
	// markdownLink is [label](url): the label stays in the spoken text.
	markdownLink = regexp.MustCompile(`\[([^\]\n]*)\]\((https?://[^()\s]+(?:\([^()\s]*\)[^()\s]*)*)\)`)
	// bareURL runs to whitespace, hiragana (Japanese written right after the
	// URL: …/を見てね, …/です) or a character no URL in an answer carries
	// (Japanese brackets and punctuation, < >). Kanji and katakana stay:
	// they are in page names (wiki/東京タワー).
	bareURL = regexp.MustCompile(`<?https?://[^\s<>「」『』（）、。！？\p{Hiragana}]+>?`)
	// emptyBrackets is what a URL in brackets leaves behind.
	emptyBrackets = strings.NewReplacer("（）", "", "()", "", "<>", "", "「」", "")
)

// urlEnd is the URL in a bare match: without <>, and without the trailing
// punctuation of the sentence, or a ")" that closes a bracket opened before
// the URL (a ")" opened inside it, Foo_(bar), stays).
func urlEnd(m string) (u, rest string) {
	u = strings.TrimPrefix(m, "<")
	if s, ok := strings.CutSuffix(u, ">"); ok {
		u = s
	}
	for u != "" {
		last := u[len(u)-1]
		if last == ')' && strings.Count(u, "(") >= strings.Count(u, ")") {
			break
		}
		if !strings.ContainsRune(".,;:!?'\")]", rune(last)) {
			break
		}
		rest = string(last) + rest
		u = u[:len(u)-1]
	}
	return u, rest
}

// extractURLs takes the http(s) URLs out of an answer, in order and without
// repeats, and returns the text to say without them. Only a line that held
// a URL is tidied: it loses the brackets left empty and a trailing label
// colon, and goes when nothing but a label ("URL:") or punctuation is left.
func extractURLs(answer string) (spoken string, urls []string) {
	add := func(u string) bool {
		if !validWebURL(u) {
			return false
		}
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
		return true
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(answer), "\n") {
		had := false
		line = markdownLink.ReplaceAllStringFunc(line, func(m string) string {
			sub := markdownLink.FindStringSubmatch(m)
			if !add(sub[2]) {
				return m
			}
			had = true
			return sub[1]
		})
		line = bareURL.ReplaceAllStringFunc(line, func(m string) string {
			u, rest := urlEnd(m)
			if !add(u) {
				return m
			}
			had = true
			return rest
		})
		if had {
			line = strings.TrimRight(emptyBrackets.Replace(line), " \t:：-*•→")
			if strings.Trim(line, " \t.,。、!?！？") == "" || isURLLabel(line) {
				continue
			}
		}
		lines = append(lines, line)
	}
	if len(urls) == 0 {
		return strings.TrimSpace(answer), nil
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), urls
}

// isURLLabel is a line that only introduced the URL taken out of it.
func isURLLabel(line string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Trim(line, " \t*#"))) {
	case "url", "link", "リンク", "参考", "参考url":
		return true
	}
	return false
}

// validWebURL accepts only http and https with a host: nothing else is
// handed to the browser.
func validWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// urlOffer is the URL the robot asked about, waiting for a yes until until.
// A new question replaces it (and a new ask cancels it).
type urlOffer struct {
	mu           sync.Mutex
	url          string
	since, until time.Time
}

// daemonURLOffer is set while the daemon runs; without it (one-shot) the
// URLs are only printed.
var daemonURLOffer *urlOffer

// offer starts waiting for a yes to open u. It reports the URL it replaced.
func (o *urlOffer) offer(u string, now time.Time) (replaced string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	replaced = o.dropLocked(now)
	o.url, o.since, o.until = u, now, now.Add(urlWait)
	return replaced
}

// take returns the URL when a yes given at at answers the offer, and ends
// it. A yes before the question (a touch queued earlier) does not count.
// A nil offer (no daemon) has nothing to take.
func (o *urlOffer) take(at time.Time) (string, bool) {
	if o == nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if at.Before(o.since) {
		return "", false
	}
	u := o.dropLocked(at)
	return u, u != ""
}

// cancel ends the offer without opening; it reports the URL it dropped.
func (o *urlOffer) cancel(now time.Time) string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dropLocked(now)
}

// dropLocked ends the offer and returns its URL, or "" if none waited at now.
func (o *urlOffer) dropLocked(now time.Time) string {
	u := o.url
	o.url = ""
	if now.After(o.until) {
		return ""
	}
	return u
}

// What the robot says once it has the answer, so the user knows it was taken.
const (
	askURLOpened   = "開いたよ。"
	askURLDeclined = "わかった、開かないね。"
)

// sayReply says one of those (a fake in tests). It leaves the face as it is.
var sayReply = func(text string, e cmdEnv) {
	if err := cmdSay([]string{"--face", "", "--", text}, e); err != nil {
		fmt.Fprintf(e.errOut, "warning: ask: say %q: %v\n", text, err)
	}
}

// answerURLByTouch opens the offered URL on a head touch made at at, and
// says so in the background (the touch loop must not wait for the speech).
// It reports whether the touch was that answer.
func answerURLByTouch(c *mcp.Client, at time.Time) bool {
	u, ok := daemonURLOffer.take(at)
	if !ok {
		return false
	}
	fmt.Fprintf(os.Stderr, "daemon: ask: answered by touch\n")
	if openAndLog(u, os.Stderr) == nil {
		go sayReply(askURLOpened, logEnv(c))
	}
	return true
}

// A spoken yes to "ページを開く？": the pomodoro's yes, and the ways to
// say "open it" ("開いて", "うん", "お願いします").
var (
	openWords   = append(slices.Clone(answerWords), "開いて", "ひらいて", "開く", "ひらく", "うん", "いいよ", "お願い", "おねがい")
	openEndings = append(slices.Clone(answerEndings), "ください", "します")
)

// A spoken no ends the offer: "いいえ", "開かないで", "いらないです".
var (
	declineWords   = []string{"いいえ", "いや", "いらない", "開かない", "ひらかない", "やめて", "やめとく", "やめておく", "no", "のー"}
	declineEndings = []string{"", "で", "です", "よ", "ね"}
)

func isDecline(heard string) bool { return isShortReply(heard, declineWords, declineEndings) }

// answerURLByVoice takes heard as the answer to the offered URL, if one
// waits and heard is a short yes or no. It reports whether it was.
func answerURLByVoice(heard string, e cmdEnv) bool {
	now := time.Now()
	switch {
	case isShortReply(heard, openWords, openEndings):
		if u, ok := daemonURLOffer.take(now); ok {
			fmt.Fprintf(e.errOut, "voice: heard %q -> open the page asked about\n", heard)
			if openAndLog(u, e.errOut) == nil {
				sayReply(askURLOpened, e)
			}
			return true
		}
	case isDecline(heard):
		if u := daemonURLOffer.cancel(now); u != "" {
			fmt.Fprintf(e.errOut, "voice: heard %q -> not opening %s\n", heard, u)
			sayReply(askURLDeclined, e)
			return true
		}
	}
	return false
}

// cmdAskOpen is `ask --open`: yes to the robot's question.
func cmdAskOpen(e cmdEnv) error {
	if daemonURLOffer == nil {
		return errors.New("ask --open needs a running daemon")
	}
	u, ok := daemonURLOffer.take(time.Now())
	if !ok {
		fmt.Fprintln(e.out, "ask: no page is asked about")
		return nil
	}
	fmt.Fprintln(os.Stderr, "daemon: ask: answered by ask --open")
	if err := openAndLog(u, os.Stderr); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "ask: opened %s\n", u)
	return nil
}

// openAndLog opens u in the browser and logs it to w.
func openAndLog(u string, w io.Writer) error {
	if err := openBrowser(u); err != nil {
		err = fmt.Errorf("ask: open %s: %w", u, err)
		fmt.Fprintf(w, "warning: %v\n", err)
		return err
	}
	fmt.Fprintf(w, "ask: opened %s\n", u)
	return nil
}

// openBrowser opens an http(s) URL in the default browser (a fake in
// tests). On Windows it goes through url.dll, not "cmd /c start", which
// would read & in the URL. Like openInViewer, it starts outside our
// kill-on-exit job, so a browser it launches outlives the daemon.
var openBrowser = func(u string) error {
	if !validWebURL(u) {
		return errors.New("not an http(s) URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return startDetached(cmd)
}
