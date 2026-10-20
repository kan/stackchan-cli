package main

// Timers and the pomodoro, held by the daemon (in memory: a restart drops
// them). `timer 3m` speaks when the time is up. `pomodoro start` runs work
// (25 min) and breaks (5 min, every 4th 15 min): during each the 12 LEDs are
// a dim gauge of the time left, which turns yellow at 10 and orange at 5
// minutes left (the quiet notices). The end of work is announced loudly
// (voice, face, LEDs, head) and repeated until answered by a head touch or
// `pomodoro ok` (voice: はい), which starts the break; the end of a break is
// announced and answered the same way, which starts the next work.
//
// timerSet is the state machine; it takes the time as an argument, so tests
// step through 25 minutes at once. runTimers turns its events into what the
// device does: the reaction scripts in defaultTimerScripts.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"stackchan-cli/internal/mcp"
)

// ---- durations --------------------------------------------------------------

// durPattern matches a spoken duration in normalized speech (normalizeSpeech):
// "3分", "三分", "1時間半", "1分30秒", "二十五分". Whisper writes numbers
// both ways.
const durPattern = `(?:(?:[0-9]+|[〇零一二三四五六七八九十百]+)(?:時間|分|秒)半?)+`

var (
	durWholeRe = regexp.MustCompile(`^` + durPattern + `$`)
	durPartRe  = regexp.MustCompile(`([0-9]+|[〇零一二三四五六七八九十百]+)(時間|分|秒)(半?)`)
	durUnits   = map[string]time.Duration{"時間": time.Hour, "分": time.Minute, "秒": time.Second}
	kanjiDigit = map[rune]int{'〇': 0, '零': 0, '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	kanjiUnit  = map[rune]int{'十': 10, '百': 100}
)

// parseSpokenDuration reads a duration durPattern matches, the whole of s.
func parseSpokenDuration(s string) (time.Duration, bool) {
	if !durWholeRe.MatchString(s) {
		return 0, false
	}
	var d time.Duration
	for _, p := range durPartRe.FindAllStringSubmatch(s, -1) {
		n, ok := parseNumber(p[1])
		if !ok {
			return 0, false
		}
		unit := durUnits[p[2]]
		d += time.Duration(n) * unit
		if p[3] == "半" {
			d += unit / 2
		}
	}
	return d, d > 0
}

// parseNumber reads ASCII digits or kanji numerals: 二十五 = 25, 十 = 10,
// 百二十 = 120, 一〇 = 10.
func parseNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	total, cur := 0, -1 // cur -1: no digit since the last 十 / 百
	for _, r := range s {
		if unit, ok := kanjiUnit[r]; ok {
			total += max(cur, 1) * unit
			cur = -1
			continue
		}
		d, ok := kanjiDigit[r]
		if !ok {
			return 0, false
		}
		cur = max(cur, 0)*10 + d
	}
	return total + max(cur, 0), true
}

// parseTimerDuration reads a timer's length: Go's form ("3m", "1h30m",
// "90s") or a spoken one ("3分").
func parseTimerDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		var ok bool
		if d, ok = parseSpokenDuration(normalizeSpeech(s)); !ok {
			return 0, fmt.Errorf("bad duration %q (e.g. 3m, 1h30m, 90s, 3分)", s)
		}
	}
	if d < time.Second || d > 24*time.Hour {
		return 0, fmt.Errorf("duration %s: want 1s..24h", d)
	}
	return d, nil
}

// formatDur writes d as a timer command takes it: "3m", "1h30m", "1m30s".
func formatDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if t, ok := strings.CutSuffix(s, "m0s"); ok {
		s = t + "m"
	}
	if t, ok := strings.CutSuffix(s, "h0m"); ok {
		s = t + "h"
	}
	return s
}

// spokenDur says d: "3分", "1時間30分", "1分30秒", "45秒".
func spokenDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%d時間", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%d分", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%d秒", s)
	}
	return b.String()
}

// spokenLeft says the time left, finer as it gets short: "12分", "3分20秒".
func spokenLeft(d time.Duration) string {
	if d >= 10*time.Minute {
		d = d.Round(time.Minute)
	}
	return spokenDur(d)
}

// ---- the state machine ------------------------------------------------------

type pomoConfig struct {
	work, short, long time.Duration
	longEvery         int // every n-th break is long
}

var defaultPomoConfig = pomoConfig{work: 25 * time.Minute, short: 5 * time.Minute, long: 15 * time.Minute, longEvery: 4}

type pomoPhase int

const (
	pomoWork      pomoPhase = iota
	pomoWaitBreak           // work is over; waiting for an answer to start the break
	pomoBreak
	pomoWaitWork // the break is over; waiting for an answer to start work
)

func (ph pomoPhase) waiting() bool { return ph == pomoWaitBreak || ph == pomoWaitWork }

// The loud notice at the end of work or a break is repeated while
// unanswered, a few times, then the waiting goes on silently (the LEDs stay
// red, or green after a break).
const (
	pomoRemindEvery = 3 * time.Minute
	pomoReminders   = 5
)

type pomodoro struct {
	cfg       pomoConfig
	phase     pomoPhase
	round     int       // the work session in progress or just done, from 1
	end       time.Time // of work or break
	breakLen  time.Duration
	reminded  int       // reminders said while waiting
	lastSaid  time.Time // the notice or reminder said last
	waitSince time.Time // waiting began: an answer must come after it
	lastGauge gauge
}

// phaseLen is the length of work or the break in progress.
func (p *pomodoro) phaseLen() time.Duration {
	if p.phase == pomoBreak {
		return p.breakLen
	}
	return p.cfg.work
}

type plainTimer struct {
	id  int
	len time.Duration
	end time.Time
}

// gauge is what the LEDs show: lit of 12, in color.
type gauge struct {
	lit   int
	color [3]int
}

// Gauge colors, dim so they stay in the corner of the eye, except the bright
// ones of waiting for an answer.
var (
	gaugeWork      = [3]int{0, 25, 60}
	gaugeTen       = [3]int{60, 45, 0} // 10 minutes left
	gaugeFive      = [3]int{80, 25, 0} // 5 minutes left
	gaugeBreak     = [3]int{0, 60, 20}
	gaugeWaitBreak = [3]int{255, 0, 0}  // work is over
	gaugeWaitWork  = [3]int{0, 200, 60} // the break is over
)

type timerEventKind int

const (
	evTimerDone   timerEventKind = iota // a plain timer is up
	evWorkDone                          // work is over: loud notice, wait for an answer
	evWorkRemind                        // still no answer to the end of work
	evBreakDone                         // the break is over: loud notice, wait for an answer
	evBreakRemind                       // still no answer to the end of the break
	evBreakStart                        // answered: announce the break
	evWorkStart                         // answered: announce work
	evGauge                             // redraw the LEDs
	evClear                             // the pomodoro stopped: LEDs off
)

type timerEvent struct {
	kind  timerEventKind
	dur   time.Duration // evTimerDone: the timer's length; evWorkDone, evBreakStart: the break's; evWorkStart: work's
	round int           // evWorkDone: the work session done; evBreakDone, evWorkStart: the next one
	gauge gauge         // evGauge
}

// timerSet holds the plain timers and the pomodoro.
//
// What commands and touches change takes effect at once, but what it says
// or shows waits for the next advance (announce, clearLEDs), so it runs in
// order with the notices: the break is not announced during the
// end-of-work notice, and the LEDs are not cleared under a gauge being drawn.
type timerSet struct {
	mu        sync.Mutex
	nextID    int
	plain     []plainTimer
	pomo      *pomodoro
	redraw    bool        // the LEDs were used for something else: draw the gauge again
	announce  *timerEvent // answered: evBreakStart or evWorkStart to say
	clearLEDs bool        // stopped: turn the gauge off
}

// daemonTimers is set by the daemon before it serves commands; nil elsewhere.
var daemonTimers *timerSet

func (t *timerSet) addTimer(now time.Time, d time.Duration) plainTimer {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	p := plainTimer{id: t.nextID, len: d, end: now.Add(d)}
	t.plain = append(t.plain, p)
	return p
}

// cancelTimers drops every plain timer (not the pomodoro) and says how many.
func (t *timerSet) cancelTimers() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.plain)
	t.plain = nil
	return n
}

// startPomodoro starts work, unless a pomodoro is running (false).
func (t *timerSet) startPomodoro(now time.Time, cfg pomoConfig) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pomo != nil {
		return false
	}
	t.pomo = &pomodoro{cfg: cfg, phase: pomoWork, round: 1, end: now.Add(cfg.work)}
	t.redraw = true
	return true
}

func (t *timerSet) stopPomodoro() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	running := t.pomo != nil
	t.pomo = nil
	t.clearLEDs = t.clearLEDs || running
	t.announce = nil
	return running
}

// answer moves on when the pomodoro waits for an answer given at now: to the
// break after work, to the next work after a break. started is what began
// (evBreakStart or evWorkStart); ok is false when nothing waits, or the
// answer came before the waiting began (a touch queued during a reaction).
// With announce, the next advance announces it.
func (t *timerSet) answer(now time.Time, announce bool) (started timerEvent, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pomo
	if p == nil || !p.phase.waiting() || now.Before(p.waitSince) {
		return timerEvent{}, false
	}
	if p.phase == pomoWaitBreak {
		p.phase, p.end = pomoBreak, now.Add(p.breakLen)
		started = timerEvent{kind: evBreakStart, dur: p.breakLen}
	} else {
		p.round++
		p.phase, p.end = pomoWork, now.Add(p.cfg.work)
		started = timerEvent{kind: evWorkStart, round: p.round, dur: p.cfg.work}
	}
	t.redraw = true
	if announce {
		t.announce = &started
	}
	return started, true
}

// describe says what an answer started, for the log: "break 5m", "work 2 (25m)".
func (ev timerEvent) describe() string {
	if ev.kind == evWorkStart {
		return fmt.Sprintf("work %d (%s)", ev.round, formatDur(ev.dur))
	}
	return "break " + formatDur(ev.dur)
}

// waiting reports whether the pomodoro waits for an answer.
func (t *timerSet) waiting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pomo != nil && t.pomo.phase.waiting()
}

// stale reports, just before ev is acted on, that it no longer applies:
// while it waited for stepMu the pomodoro stopped, was answered, or
// StackChan fell asleep (a gauge is then drawn again on waking).
func (t *timerSet) stale(ev timerEvent, asleep bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pomo
	switch ev.kind {
	case evGauge:
		if asleep {
			t.redraw = true
		}
		return asleep || p == nil
	case evWorkDone, evWorkRemind:
		return p == nil || p.phase != pomoWaitBreak
	case evBreakDone, evBreakRemind:
		return p == nil || p.phase != pomoWaitWork
	case evBreakStart, evWorkStart:
		return p == nil
	}
	return false
}

// ledsUsed says something else (a touch reaction, a warning) used the LEDs,
// so the gauge is drawn again.
func (t *timerSet) ledsUsed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.redraw = true
}

// advance moves everything to now and returns what to announce or draw.
// While asleep the gauge is not drawn (sleep.txt turned the LEDs off); it is
// drawn on the first call after waking.
func (t *timerSet) advance(now time.Time, asleep bool) []timerEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	var evs []timerEvent
	if t.clearLEDs {
		t.clearLEDs = false
		evs = append(evs, timerEvent{kind: evClear})
	}
	if t.announce != nil {
		evs = append(evs, *t.announce)
		t.announce = nil
	}
	kept := t.plain[:0]
	for _, p := range t.plain {
		if now.Before(p.end) {
			kept = append(kept, p)
			continue
		}
		evs = append(evs, timerEvent{kind: evTimerDone, dur: p.len})
		t.redraw = true // the notice uses the LEDs
	}
	t.plain = kept
	p := t.pomo
	if p == nil {
		return evs
	}
	switch {
	case p.phase == pomoWork && !now.Before(p.end):
		p.breakLen = p.cfg.short
		if p.cfg.longEvery > 0 && p.round%p.cfg.longEvery == 0 {
			p.breakLen = p.cfg.long
		}
		p.phase, p.reminded, p.lastSaid, p.waitSince = pomoWaitBreak, 0, now, now
		evs = append(evs, timerEvent{kind: evWorkDone, dur: p.breakLen, round: p.round})
	case p.phase == pomoBreak && !now.Before(p.end):
		p.phase, p.reminded, p.lastSaid, p.waitSince = pomoWaitWork, 0, now, now
		evs = append(evs, timerEvent{kind: evBreakDone, round: p.round + 1})
	case p.phase.waiting() && !asleep && p.reminded < pomoReminders && now.Sub(p.lastSaid) >= pomoRemindEvery:
		// Asleep, the notice sounded, but the reminders wait (nobody is there,
		// and the touch sensor that would answer them is off).
		p.reminded++
		p.lastSaid = now
		kind := evWorkRemind
		if p.phase == pomoWaitWork {
			kind = evBreakRemind
		}
		evs = append(evs, timerEvent{kind: kind})
	}
	if asleep {
		t.redraw = true
		return evs
	}
	if g, ok := p.gauge(now); ok && (t.redraw || g != p.lastGauge) {
		p.lastGauge = g
		t.redraw = false
		evs = append(evs, timerEvent{kind: evGauge, gauge: g})
	}
	return evs
}

// gauge is the LEDs for work or a break: one LED per 1/12 of the phase
// left (rounded up), colored by the phase and, in work, the time left.
// Waiting for an answer, all 12 are red (work is over) or green (the break).
func (p *pomodoro) gauge(now time.Time) (gauge, bool) {
	switch p.phase {
	case pomoWaitBreak:
		return gauge{12, gaugeWaitBreak}, true
	case pomoWaitWork:
		return gauge{12, gaugeWaitWork}, true
	}
	total, left := p.phaseLen(), p.end.Sub(now)
	if total <= 0 {
		return gauge{}, false
	}
	lit := int(math.Ceil(12 * float64(left) / float64(total)))
	g := gauge{lit: min(max(lit, 0), 12), color: gaugeBreak}
	if p.phase == pomoWork {
		switch {
		case left <= 5*time.Minute:
			g.color = gaugeFive
		case left <= 10*time.Minute:
			g.color = gaugeTen
		default:
			g.color = gaugeWork
		}
	}
	return g, true
}

// status lists what runs, as spoken text ("" when nothing does).
func (t *timerSet) status(now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var parts []string
	for _, p := range t.plain {
		parts = append(parts, fmt.Sprintf("%sのタイマーは、あと%s。", spokenDur(p.len), spokenLeft(p.end.Sub(now))))
	}
	if p := t.pomo; p != nil {
		switch p.phase {
		case pomoWork:
			parts = append(parts, fmt.Sprintf("ポモドーロは%d回目の作業中で、あと%s。", p.round, spokenLeft(p.end.Sub(now))))
		case pomoWaitBreak:
			parts = append(parts, "ポモドーロは、休憩の合図を待ってるよ。")
		case pomoWaitWork:
			parts = append(parts, "ポモドーロは、作業を始める合図を待ってるよ。")
		case pomoBreak:
			parts = append(parts, fmt.Sprintf("ポモドーロは休憩中で、あと%s。", spokenLeft(p.end.Sub(now))))
		}
	}
	return strings.Join(parts, "")
}

// ---- running it in the daemon ----------------------------------------------

// defaultTimerScripts seed reactions/<name>.txt: what the device does for each
// notice, editable like the touch reactions. {timer} is the timer's length,
// {round} the work session done or starting, {break} the break's length,
// {work} work's length (all as spoken, e.g. 3分).
var defaultTimerScripts = map[string]string{
	"timer-done": `# タイマーの時間が来たとき。{timer} はタイマーの長さ（例: 3分）。
all-leds --r 255 --g 160 --b 0
say --while-face surprised "{timer}たったよ。"
clear-leds
`,
	"pomodoro-work-done": `# ポモドーロの作業が終わったとき。このあと応答（頭のタッチ、はい）を待つ間、LED は 12 個とも赤になる。
# {round} は何回目の作業か、{break} は次の休憩の長さ。
all-leds --r 255 --g 0 --b 0
move-head --yaw -25 --pitch 45
move-head --yaw 25 --pitch 45
move-head --yaw 0 --pitch 45
say --while-face surprised --face happy "{round}回目の作業おわり。{break}休憩しよう。頭をタッチするか、はいって言ってね。"
`,
	"pomodoro-remind": `# 作業の終わりに応答が無いとき（3 分ごとに 5 回まで）。
all-leds --r 255 --g 0 --b 0
move-head --yaw -25 --pitch 45
move-head --yaw 25 --pitch 45
move-head --yaw 0 --pitch 45
say --while-face surprised --face happy "休憩しよう。頭をタッチしてね。"
`,
	"pomodoro-break-start": `# 応答して休憩が始まるとき。LED はこのあと休憩の目盛りになる。{break} は休憩の長さ。
say --face happy "{break}休憩ね。ゆっくりしてね。"
`,
	"pomodoro-break-done": `# 休憩が終わったとき。このあと応答（頭のタッチ、はい）を待つ間、LED は 12 個とも緑になる。
# {round} は次が何回目の作業か。
all-leds --r 0 --g 200 --b 60
move-head --yaw -25 --pitch 45
move-head --yaw 25 --pitch 45
move-head --yaw 0 --pitch 45
say --while-face surprised --face happy "休憩おわり。{round}回目の作業を始めよう。頭をタッチするか、はいって言ってね。"
`,
	"pomodoro-break-remind": `# 休憩の終わりに応答が無いとき（3 分ごとに 5 回まで）。
all-leds --r 0 --g 200 --b 60
move-head --yaw -25 --pitch 45
move-head --yaw 25 --pitch 45
move-head --yaw 0 --pitch 45
say --while-face surprised --face happy "作業に戻ろう。頭をタッチしてね。"
`,
	"pomodoro-work-start": `# 応答して作業が始まるとき。LED はこのあと作業の目盛りになる。{round} は何回目の作業か、{work} は作業の長さ。
say --face happy "{round}回目の作業、始めるよ。{work}がんばろう。"
`,
}

// runTimers advances the timers every second and acts on the events. The
// notices run under stepMu, so they do not mix with a touch reaction; they
// sound while asleep too (a timer was set on purpose).
func runTimers(c *mcp.Client, t *timerSet, sl *sleeper, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		for _, ev := range t.advance(time.Now(), sl.asleep.Load()) {
			sl.serialized(func() {
				// Waiting for stepMu, the pomodoro may have stopped, been
				// answered, or sleep begun.
				if t.stale(ev, sl.asleep.Load()) {
					return
				}
				actOnTimer(c, ev)
			})
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// actOnTimer does what an event asks of the device. Output goes to the log.
func actOnTimer(c *mcp.Client, ev timerEvent) {
	switch ev.kind {
	case evGauge:
		setGauge(c, ev.gauge)
	case evTimerDone:
		fmt.Fprintf(os.Stderr, "daemon: timer %s is up\n", formatDur(ev.dur))
		runTimerScript(c, "timer-done", "{timer}", spokenDur(ev.dur))
	case evWorkDone:
		fmt.Fprintf(os.Stderr, "daemon: pomodoro: work %d done; waiting for an answer\n", ev.round)
		runTimerScript(c, "pomodoro-work-done", "{round}", strconv.Itoa(ev.round), "{break}", spokenDur(ev.dur))
	case evWorkRemind:
		fmt.Fprintln(os.Stderr, "daemon: pomodoro: reminding to take the break (no answer yet)")
		runTimerScript(c, "pomodoro-remind")
	case evBreakDone:
		fmt.Fprintf(os.Stderr, "daemon: pomodoro: break done; waiting for an answer to start work %d\n", ev.round)
		runTimerScript(c, "pomodoro-break-done", "{round}", strconv.Itoa(ev.round))
	case evBreakRemind:
		fmt.Fprintln(os.Stderr, "daemon: pomodoro: reminding to start work (no answer yet)")
		runTimerScript(c, "pomodoro-break-remind")
	case evBreakStart:
		runTimerScript(c, "pomodoro-break-start", "{break}", spokenDur(ev.dur))
	case evWorkStart:
		runTimerScript(c, "pomodoro-work-start", "{round}", strconv.Itoa(ev.round), "{work}", spokenDur(ev.dur))
	case evClear:
		if _, err := callTool(c, "clear_leds", map[string]any{}, nil); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: timer gauge: %v\n", err)
		}
	}
}

func runTimerScript(c *mcp.Client, name string, vars ...string) {
	runNamedScript(c, name, defaultTimerScripts, false, strings.NewReplacer(vars...))
}

// setGauge shows g on the 12 LEDs in one call.
func setGauge(c *mcp.Client, g gauge) {
	colors := make([][3]int, 12)
	for i := range g.lit {
		colors[i] = g.color
	}
	if _, err := callTool(c, "set_leds", map[string]any{"colors": colors}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: timer gauge: %v\n", err)
	}
}

// answerByTouch takes a head touch as the answer when the pomodoro waits
// for one, and starts the break or work. It reports whether it did.
// at is when the touch was made: one made before the waiting began (queued
// during a reaction) is not an answer.
func answerByTouch(t *timerSet, at time.Time) bool {
	started, ok := t.answer(at, true)
	if ok {
		fmt.Fprintf(os.Stderr, "daemon: pomodoro: answered by touch; %s\n", started.describe())
	}
	return ok
}

// A spoken answer is one of answerWords, said once or twice, and at most
// one of answerEndings: "はい", "はいはい", "了解です", "わかったよ". It is
// the whole utterance (after the name), so "はい" or "ok" inside other
// speech (はいらない, tokyo) is not an answer. Taken only while the pomodoro
// waits for one.
var (
	answerWords   = []string{"はい", "おっけー", "おーけー", "ok", "わかった", "りょうかい", "了解"}
	answerEndings = []string{"", "です", "だよ", "よ", "ね", "しました"}
)

// isAnswer reports whether heard reads as a short yes.
func isAnswer(heard string) bool {
	h := strings.TrimPrefix(normalizeSpeech(heard), "すたっくちゃん")
	for _, w := range answerWords {
		rest, ok := strings.CutPrefix(h, w)
		if !ok {
			continue
		}
		rest = strings.TrimPrefix(rest, w)
		if slices.Contains(answerEndings, rest) {
			return true
		}
	}
	return false
}

// ---- commands ---------------------------------------------------------------

var errNoDaemonTimers = errors.New("timers need a running daemon (stackchan-cli daemon)")

// confirm speaks a command's confirmation unless --quiet.
func confirm(quiet bool, text string, e cmdEnv) error {
	if quiet {
		return nil
	}
	return cmdSay([]string{text}, e)
}

// statusOr prints the status, or none when nothing runs, and returns it.
func statusOr(t *timerSet, none string, e cmdEnv) string {
	s := t.status(time.Now())
	if s == "" {
		s = none
	}
	fmt.Fprintln(e.out, s)
	return s
}

// cmdTimer is `timer <duration>` (start), `timer` (list), `timer --say`
// (speak what runs) and `timer cancel`.
func cmdTimer(args []string, e cmdEnv) error {
	set, _ := fs("timer")
	quiet := set.Bool("quiet", false, "do not speak the confirmation")
	speak := set.Bool("say", false, "without a duration: speak the time left")
	pos, err := parseInterspersed(set, args)
	if err != nil {
		return err
	}
	t := daemonTimers
	if t == nil {
		return errNoDaemonTimers
	}
	switch {
	case len(pos) == 0:
		s := statusOr(t, "動いてるタイマーはないよ。", e)
		return confirm(!*speak, s, e)
	case pos[0] == "cancel":
		n := t.cancelTimers()
		fmt.Fprintf(e.out, "cancelled %d timer(s)\n", n)
		if n == 0 {
			return confirm(*quiet, "動いてるタイマーはないよ。", e)
		}
		return confirm(*quiet, "タイマーを止めたよ。", e)
	case len(pos) > 1:
		return fmt.Errorf("timer: one duration at a time, got %q", pos)
	default:
		d, err := parseTimerDuration(pos[0])
		if err != nil {
			return err
		}
		p := t.addTimer(time.Now(), d)
		fmt.Fprintf(e.out, "timer %d: %s (until %s)\n", p.id, formatDur(d), p.end.Format("15:04:05"))
		return confirm(*quiet, spokenDur(d)+"のタイマーを始めるよ。", e)
	}
}

// cmdPomodoro is `pomodoro start|stop|ok` and `pomodoro` (status).
func cmdPomodoro(args []string, e cmdEnv) error {
	set, _ := fs("pomodoro")
	cfg := defaultPomoConfig
	set.DurationVar(&cfg.work, "work", cfg.work, "work length")
	set.DurationVar(&cfg.short, "break", cfg.short, "break length")
	set.DurationVar(&cfg.long, "long", cfg.long, "long break length")
	set.IntVar(&cfg.longEvery, "every", cfg.longEvery, "every n-th break is long (0 = never)")
	quiet := set.Bool("quiet", false, "do not speak the confirmation")
	pos, err := parseInterspersed(set, args)
	if err != nil {
		return err
	}
	t := daemonTimers
	if t == nil {
		return errNoDaemonTimers
	}
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "":
		statusOr(t, "ポモドーロは動いてないよ。", e)
		return nil
	case "start":
		if cfg.work < time.Second || cfg.short < time.Second || cfg.long < time.Second || cfg.longEvery < 0 {
			return errors.New("pomodoro: lengths must be at least 1s, --every 0 or more")
		}
		if !t.startPomodoro(time.Now(), cfg) {
			fmt.Fprintln(e.out, "pomodoro: already running (lengths unchanged; stop it first to change them)")
			return confirm(*quiet, "ポモドーロはもう始まってるよ。"+t.status(time.Now()), e)
		}
		fmt.Fprintf(e.out, "pomodoro: work %s, break %s (every %d-th %s)\n", formatDur(cfg.work), formatDur(cfg.short), cfg.longEvery, formatDur(cfg.long))
		// Not "ポモドーロを": VOICEVOX blends を into the last ロ (ポモド'オロオ).
		return confirm(*quiet, fmt.Sprintf("ポモドーロ、始めるよ。%sがんばろう。", spokenDur(cfg.work)), e)
	case "stop":
		if !t.stopPomodoro() {
			fmt.Fprintln(e.out, "pomodoro: not running")
			return confirm(*quiet, "ポモドーロは動いてないよ。", e)
		}
		fmt.Fprintln(e.out, "pomodoro: stopped")
		return confirm(*quiet, "ポモドーロおわり。おつかれさま。", e)
	case "ok":
		started, ok := t.answer(time.Now(), !*quiet)
		if !ok {
			fmt.Fprintln(e.out, "pomodoro: nothing waits for an answer")
			return confirm(*quiet, "今は返事を待ってないよ。", e)
		}
		// The daemon announces it (reactions/pomodoro-break-start.txt or
		// pomodoro-work-start.txt) once the notice asking for this answer is over.
		fmt.Fprintf(e.out, "pomodoro: %s\n", started.describe())
		return nil
	default:
		return fmt.Errorf("pomodoro: want start | stop | ok, got %q", sub)
	}
}
