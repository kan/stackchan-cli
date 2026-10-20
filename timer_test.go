package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseNumber(t *testing.T) {
	for s, want := range map[string]int{
		"3": 3, "25": 25, "三": 3, "十": 10, "十五": 15, "二十": 20, "二十五": 25,
		"百": 100, "百二十": 120, "一〇": 10, "〇": 0,
	} {
		if got, ok := parseNumber(s); !ok || got != want {
			t.Errorf("parseNumber(%q) = %d, %v; want %d", s, got, ok, want)
		}
	}
	if _, ok := parseNumber("何"); ok {
		t.Error("parseNumber(何) accepted it")
	}
}

func TestParseSpokenDuration(t *testing.T) {
	for s, want := range map[string]time.Duration{
		"3分":     3 * time.Minute,
		"三分":     3 * time.Minute,
		"十秒":     10 * time.Second,
		"二十五分":   25 * time.Minute,
		"1時間半":   90 * time.Minute,
		"1分半":    90 * time.Second,
		"1時間30分": 90 * time.Minute,
		"1分30秒":  90 * time.Second,
		"一時間":    time.Hour,
	} {
		if got, ok := parseSpokenDuration(s); !ok || got != want {
			t.Errorf("parseSpokenDuration(%q) = %s, %v; want %s", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "分", "3", "何分", "0分", "3分くらい"} {
		if d, ok := parseSpokenDuration(s); ok {
			t.Errorf("parseSpokenDuration(%q) = %s, want not ok", s, d)
		}
	}
}

func TestParseTimerDuration(t *testing.T) {
	for s, want := range map[string]time.Duration{
		"3m": 3 * time.Minute, "1h30m": 90 * time.Minute, "90s": 90 * time.Second, "3分": 3 * time.Minute, "１時間半": 90 * time.Minute,
	} {
		if got, err := parseTimerDuration(s); err != nil || got != want {
			t.Errorf("parseTimerDuration(%q) = %s, %v; want %s", s, got, err, want)
		}
	}
	for _, s := range []string{"0s", "500ms", "25h", "abc", ""} {
		if _, err := parseTimerDuration(s); err == nil {
			t.Errorf("parseTimerDuration(%q) accepted it", s)
		}
	}
}

func TestFormatAndSpeakDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Minute: "3m", 90 * time.Minute: "1h30m", 90 * time.Second: "1m30s", time.Hour: "1h", 10 * time.Second: "10s",
	} {
		if got := formatDur(d); got != want {
			t.Errorf("formatDur(%s) = %q, want %q", d, got, want)
		}
		// What formatDur writes, timer takes back.
		if back, err := parseTimerDuration(formatDur(d)); err != nil || back != d {
			t.Errorf("parseTimerDuration(formatDur(%s)) = %s, %v", d, back, err)
		}
	}
	for d, want := range map[time.Duration]string{
		3 * time.Minute: "3分", 90 * time.Minute: "1時間30分", 90 * time.Second: "1分30秒", 45 * time.Second: "45秒", 0: "0秒",
	} {
		if got := spokenDur(d); got != want {
			t.Errorf("spokenDur(%s) = %q, want %q", d, got, want)
		}
	}
	if got := spokenLeft(12*time.Minute + 20*time.Second); got != "12分" {
		t.Errorf("spokenLeft(12m20s) = %q, want 12分 (minutes only when it is long)", got)
	}
	if got := spokenLeft(3*time.Minute + 20*time.Second); got != "3分20秒" {
		t.Errorf("spokenLeft(3m20s) = %q", got)
	}
}

func TestVoiceRulesDuration(t *testing.T) {
	vr, err := parseVoiceRules(defaultVoiceCommands)
	if err != nil {
		t.Fatal(err)
	}
	for heard, want := range map[string]string{
		"3分タイマー": "timer 3m",
		"スタックチャン、三分のタイマー": "timer 3m",
		"1時間半タイマーお願い":     "timer 1h30m",
		"十秒タイマー":          "timer 10s",
		"タイマー止めて":         "timer cancel",
		"ポモドーロ始めるよ":       "pomodoro start",
		"ポモドーロ終わり":        "pomodoro stop",
		"あと何分？":           "timer --say",
		"ポモドーロあと何分？":      "timer --say",
		"今何時何分？":          `say "{date}、{weekday}、{time}です"`,
	} {
		if got, _ := vr.match(heard); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s -> %q, want %q", heard, got, want)
		}
	}
	// A timer without a time is not a timer.
	if got, kw := vr.match("タイマー"); kw != "" || !reflect.DeepEqual(got, vr.fallback) {
		t.Errorf("タイマー -> %q (%q), want the * rule", got, kw)
	}
	if _, err := parseVoiceRules("{dur}と{dur} => timer {dur}\n"); err == nil {
		t.Error("two {dur} in a keyword were accepted")
	}
	// {dur} rules give no prompt word of their own beyond the text around it.
	only, err := parseVoiceRules("{dur}タイマー => timer {dur}\n")
	if err != nil || only.sttPrompt() != "スタックチャン、タイマー。" {
		t.Errorf("prompt = %q, %v", only.sttPrompt(), err)
	}
}

// awake is advance while StackChan is awake.
func (t *timerSet) awake(now time.Time) []timerEvent { return t.advance(now, false) }

func kinds(evs []timerEvent) []timerEventKind {
	var k []timerEventKind
	for _, e := range evs {
		k = append(k, e.kind)
	}
	return k
}

func TestPomodoroCycle(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	var ts timerSet
	if !ts.startPomodoro(t0, defaultPomoConfig) {
		t.Fatal("start failed")
	}
	if ts.startPomodoro(t0, defaultPomoConfig) {
		t.Error("a second start was accepted")
	}

	// The gauge: all 12 at the start, fewer as time passes, yellow at 10 and
	// orange at 5 minutes left; only changes are drawn.
	evs := ts.awake(t0)
	if len(evs) != 1 || evs[0].kind != evGauge || evs[0].gauge != (gauge{12, gaugeWork}) {
		t.Fatalf("start: %+v", evs)
	}
	if evs := ts.awake(t0.Add(time.Second)); len(evs) != 0 {
		t.Errorf("no change, yet %+v", evs)
	}
	if evs := ts.awake(t0.Add(15 * time.Minute)); len(evs) != 1 || evs[0].gauge != (gauge{5, gaugeTen}) {
		t.Errorf("10 minutes left: %+v", evs)
	}
	if evs := ts.awake(t0.Add(20 * time.Minute)); len(evs) != 1 || evs[0].gauge != (gauge{3, gaugeFive}) {
		t.Errorf("5 minutes left: %+v", evs)
	}
	if _, ok := ts.answer(t0.Add(21*time.Minute), true); ok {
		t.Error("an answer during work was taken")
	}

	// The end of work: a loud notice, then reminders while unanswered.
	end := t0.Add(25 * time.Minute)
	evs = ts.awake(end)
	if !reflect.DeepEqual(kinds(evs), []timerEventKind{evWorkDone, evGauge}) || evs[0].dur != 5*time.Minute || evs[0].round != 1 ||
		evs[1].gauge != (gauge{12, gaugeWaitBreak}) {
		t.Fatalf("end of work: %+v, want the notice, then all LEDs red", evs)
	}
	if evs := ts.awake(end.Add(2 * time.Minute)); len(evs) != 0 {
		t.Errorf("too early for a reminder: %+v", evs)
	}
	reminders := 0
	for m := 3; m <= 60; m++ {
		for _, e := range ts.awake(end.Add(time.Duration(m) * time.Minute)) {
			if e.kind == evWorkRemind {
				reminders++
			}
		}
	}
	if reminders != pomoReminders {
		t.Errorf("%d reminders in an hour, want %d", reminders, pomoReminders)
	}

	// The answer starts the break.
	answered := end.Add(time.Hour)
	if started, ok := ts.answer(answered, true); !ok || started != (timerEvent{kind: evBreakStart, dur: 5 * time.Minute}) {
		t.Fatalf("answer = %+v, %v", started, ok)
	}
	if _, ok := ts.answer(answered, true); ok {
		t.Error("a second answer was taken")
	}
	// The break is announced by the next advance, in turn with the notices.
	evs = ts.awake(answered)
	if !reflect.DeepEqual(kinds(evs), []timerEventKind{evBreakStart, evGauge}) || evs[0].dur != 5*time.Minute || evs[1].gauge != (gauge{12, gaugeBreak}) {
		t.Errorf("break: %+v", evs)
	}

	// The end of the break waits for an answer too (green), with reminders.
	breakEnd := answered.Add(5 * time.Minute)
	evs = ts.awake(breakEnd)
	if !reflect.DeepEqual(kinds(evs), []timerEventKind{evBreakDone, evGauge}) || evs[0].round != 2 || evs[1].gauge != (gauge{12, gaugeWaitWork}) {
		t.Fatalf("end of break: %+v", evs)
	}
	if got := kinds(ts.awake(breakEnd.Add(3 * time.Minute))); !reflect.DeepEqual(got, []timerEventKind{evBreakRemind}) {
		t.Errorf("no answer after the break: %v, want a reminder", got)
	}
	if !strings.Contains(ts.status(breakEnd), "作業を始める合図を待ってる") {
		t.Errorf("status = %q", ts.status(breakEnd))
	}
	// The answer starts the next work.
	workAt := breakEnd.Add(4 * time.Minute)
	if started, ok := ts.answer(workAt, true); !ok || started != (timerEvent{kind: evWorkStart, round: 2, dur: 25 * time.Minute}) {
		t.Fatalf("answer after the break = %+v, %v", started, ok)
	}
	evs = ts.awake(workAt)
	if !reflect.DeepEqual(kinds(evs), []timerEventKind{evWorkStart, evGauge}) || evs[1].gauge != (gauge{12, gaugeWork}) {
		t.Errorf("work 2: %+v", evs)
	}
	if !strings.Contains(ts.status(workAt), "2回目の作業中で、あと25分") {
		t.Errorf("status = %q", ts.status(workAt))
	}

	// Every 4th break is long.
	now := workAt
	var breaks []time.Duration
	for range 3 {
		now = now.Add(25 * time.Minute)
		for _, e := range ts.awake(now) {
			if e.kind == evWorkDone {
				breaks = append(breaks, e.dur)
			}
		}
		started, ok := ts.answer(now, false)
		if !ok {
			t.Fatal("no answer taken")
		}
		now = now.Add(started.dur)
		ts.awake(now)
		if _, ok := ts.answer(now, false); !ok {
			t.Fatal("no answer taken after the break")
		}
	}
	if want := []time.Duration{5 * time.Minute, 5 * time.Minute, 15 * time.Minute}; !reflect.DeepEqual(breaks, want) {
		t.Errorf("breaks after work 2-4 = %v, want %v", breaks, want)
	}

	if !ts.stopPomodoro() || ts.stopPomodoro() {
		t.Error("stop: want true once, then false")
	}
	// A gauge taken out before the stop is not drawn after it; the LEDs are
	// cleared in turn.
	if !ts.stale(timerEvent{kind: evGauge}, false) || !ts.stale(timerEvent{kind: evWorkDone}, false) {
		t.Error("a gauge or a notice after the stop is not stale")
	}
	if got := kinds(ts.awake(now.Add(time.Hour))); !reflect.DeepEqual(got, []timerEventKind{evClear}) {
		t.Errorf("stopped: %v, want the LEDs cleared once", got)
	}
	if evs := ts.awake(now.Add(2 * time.Hour)); len(evs) != 0 {
		t.Errorf("stopped, yet %+v", evs)
	}
}

// Waiting for an answer, the LEDs stay red: drawn again after a timer's
// notice used them.
func TestPomodoroWaitingRedraw(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	var ts timerSet
	ts.startPomodoro(t0, defaultPomoConfig)
	ts.awake(t0.Add(25 * time.Minute))
	ts.addTimer(t0.Add(25*time.Minute), time.Minute)
	evs := ts.awake(t0.Add(26 * time.Minute))
	if !reflect.DeepEqual(kinds(evs), []timerEventKind{evTimerDone, evGauge}) || evs[1].gauge != (gauge{12, gaugeWaitBreak}) {
		t.Errorf("timer while waiting: %+v, want the red LEDs again", evs)
	}
}

// An answer is taken only after the waiting began, and a notice whose
// waiting was answered while it queued is not played; something else using
// the LEDs gets the gauge drawn again.
func TestPomodoroAnswerTiming(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	var ts timerSet
	ts.startPomodoro(t0, defaultPomoConfig)
	ts.awake(t0)
	end := t0.Add(25 * time.Minute)
	ts.awake(end)
	if _, ok := ts.answer(end.Add(-time.Second), true); ok {
		t.Error("a touch from before the end of work answered it")
	}
	if ts.stale(timerEvent{kind: evWorkDone}, false) {
		t.Error("the end-of-work notice is stale while it waits")
	}
	if _, ok := ts.answer(end.Add(time.Second), true); !ok {
		t.Fatal("the answer was not taken")
	}
	if !ts.stale(timerEvent{kind: evWorkDone}, false) || !ts.stale(timerEvent{kind: evWorkRemind}, false) {
		t.Error("a queued end-of-work notice still plays after the answer")
	}
	ts.awake(end.Add(time.Second))
	if evs := ts.awake(end.Add(2 * time.Second)); len(evs) != 0 {
		t.Errorf("no change, yet %+v", evs)
	}
	ts.ledsUsed()
	if got := kinds(ts.awake(end.Add(3 * time.Second))); !reflect.DeepEqual(got, []timerEventKind{evGauge}) {
		t.Errorf("after a reaction used the LEDs: %v, want the gauge", got)
	}
}

// Asleep, the reminders wait; on waking they go on.
func TestPomodoroNoRemindersAsleep(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 23, 30, 0, 0, time.Local)
	var ts timerSet
	ts.startPomodoro(t0, defaultPomoConfig)
	ts.awake(t0)
	end := t0.Add(25 * time.Minute)
	ts.advance(end, true)
	for m := 3; m <= 30; m += 3 {
		for _, e := range ts.advance(end.Add(time.Duration(m)*time.Minute), true) {
			if e.kind == evWorkRemind {
				t.Fatalf("a reminder at +%dm while asleep", m)
			}
		}
	}
	if got := kinds(ts.awake(end.Add(31 * time.Minute))); !reflect.DeepEqual(got, []timerEventKind{evWorkRemind, evGauge}) {
		t.Errorf("on waking: %v, want a reminder and the red LEDs", got)
	}
}

func TestIsAnswer(t *testing.T) {
	for _, s := range []string{"はい", "はい。", "ハイ!", "はいはい", "OK", "オッケー", "わかった", "わかったよ", "了解です", "スタックチャン、はい"} {
		if !isAnswer(s) {
			t.Errorf("isAnswer(%q) = false", s)
		}
	}
	for _, s := range []string{"", "東京の天気", "Tokyo", "はいらない", "灰色", "はいはい、それでね、今日の天気を教えて", "book"} {
		if isAnswer(s) {
			t.Errorf("isAnswer(%q) = true", s)
		}
	}
}

// While the pomodoro waits, a short yes heard is the answer, before the
// rules; otherwise it goes through the rules.
func TestVoiceAnswersPomodoro(t *testing.T) {
	fakeHome(t)
	seedFile(voiceCommandsPath(), "* => sleep 0\n")
	daemonTimers = &timerSet{}
	t.Cleanup(func() { daemonTimers = nil })
	var out bytes.Buffer
	e := cmdEnv{out: &out, errOut: &out}
	if err := runVoice("はい", e); err != nil || !strings.Contains(out.String(), "* rule") {
		t.Errorf("nothing waits: %v, %q; want the rules", err, out.String())
	}
	now := time.Now()
	daemonTimers.startPomodoro(now.Add(-25*time.Minute), defaultPomoConfig)
	daemonTimers.awake(now)
	out.Reset()
	if err := runVoice("はい。", e); err != nil || !strings.Contains(out.String(), "the answer the pomodoro waits for") {
		t.Errorf("waiting: %v, %q", err, out.String())
	}
	if daemonTimers.waiting() {
		t.Error("still waiting after the answer")
	}
}

func TestPlainTimers(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	var ts timerSet
	ts.addTimer(t0, 3*time.Minute)
	ts.addTimer(t0, 10*time.Minute)
	if got := ts.status(t0.Add(time.Minute)); got != "3分のタイマーは、あと2分。10分のタイマーは、あと9分。" {
		t.Errorf("status = %q", got)
	}
	if evs := ts.awake(t0.Add(3*time.Minute - time.Second)); len(evs) != 0 {
		t.Errorf("early: %+v", evs)
	}
	evs := ts.awake(t0.Add(3 * time.Minute))
	if len(evs) != 1 || evs[0].kind != evTimerDone || evs[0].dur != 3*time.Minute {
		t.Fatalf("3 minutes: %+v", evs)
	}
	if evs := ts.awake(t0.Add(4 * time.Minute)); len(evs) != 0 {
		t.Errorf("said twice: %+v", evs)
	}
	if n := ts.cancelTimers(); n != 1 {
		t.Errorf("cancelled %d, want 1", n)
	}
	if ts.status(t0) != "" {
		t.Errorf("status after cancel = %q", ts.status(t0))
	}

	// A timer's notice uses the LEDs, so the pomodoro's gauge is drawn again.
	ts.startPomodoro(t0, defaultPomoConfig)
	ts.awake(t0)
	ts.addTimer(t0, time.Minute)
	if got := kinds(ts.awake(t0.Add(time.Minute))); !reflect.DeepEqual(got, []timerEventKind{evTimerDone, evGauge}) {
		t.Errorf("timer during a pomodoro: %v, want the notice and the gauge again", got)
	}
}

// Asleep, notices still sound but the gauge is not drawn; it is drawn on the
// first advance after waking.
func TestPomodoroAsleep(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 23, 50, 0, 0, time.Local)
	var ts timerSet
	ts.startPomodoro(t0, defaultPomoConfig)
	ts.awake(t0)
	if evs := ts.advance(t0.Add(25*time.Minute), true); !reflect.DeepEqual(kinds(evs), []timerEventKind{evWorkDone}) {
		t.Errorf("end of work asleep: %v", kinds(evs))
	}
	ts.answer(t0.Add(26*time.Minute), false)
	if evs := ts.advance(t0.Add(26*time.Minute), true); len(evs) != 0 {
		t.Errorf("break asleep: %v, want no gauge", kinds(evs))
	}
	if evs := ts.awake(t0.Add(27 * time.Minute)); len(evs) != 1 || evs[0].kind != evGauge {
		t.Errorf("after waking: %v, want the gauge", kinds(evs))
	}
}

func TestTimerCommands(t *testing.T) {
	daemonTimers = nil
	var out bytes.Buffer
	e := cmdEnv{out: &out, errOut: &out}
	if err := cmdTimer([]string{"3m"}, e); !errors.Is(err, errNoDaemonTimers) {
		t.Errorf("without the daemon: %v", err)
	}
	daemonTimers = &timerSet{}
	t.Cleanup(func() { daemonTimers = nil })
	if err := cmdTimer([]string{"3分", "--quiet"}, e); err != nil {
		t.Fatal(err)
	}
	if err := cmdTimer([]string{"x"}, e); err == nil {
		t.Error("a bad duration was accepted")
	}
	if err := cmdTimer([]string{"3m", "5m"}, e); err == nil {
		t.Error("two durations were accepted")
	}
	out.Reset()
	if err := cmdTimer(nil, e); err != nil || !strings.Contains(out.String(), "3分のタイマーは") {
		t.Errorf("list: %q, %v", out.String(), err)
	}
	if err := cmdPomodoro([]string{"start", "--work", "1m", "--quiet"}, e); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdPomodoro(nil, e); err != nil || !strings.Contains(out.String(), "あと1分") {
		t.Errorf("pomodoro status: %q, %v", out.String(), err)
	}
	if err := cmdPomodoro([]string{"ok", "--quiet"}, e); err != nil {
		t.Errorf("ok with nothing waiting: %v", err)
	}
	if err := cmdPomodoro([]string{"stop", "--work", "0s", "--quiet"}, e); err != nil {
		t.Errorf("stop with a stray length flag: %v", err)
	}
	if err := cmdPomodoro([]string{"start", "--work", "0s", "--quiet"}, e); err == nil {
		t.Error("start with --work 0s was accepted")
	}
	if err := cmdPomodoro([]string{"start", "--work", "1m", "--quiet"}, e); err != nil {
		t.Fatal(err)
	}
	if err := cmdPomodoro([]string{"jump"}, e); err == nil {
		t.Error("an unknown subcommand was accepted")
	}
	if err := cmdTimer([]string{"cancel", "--quiet"}, e); err != nil || daemonTimers.status(time.Now()) == "" {
		t.Errorf("cancel: %v; the pomodoro must stay", err)
	}
}
