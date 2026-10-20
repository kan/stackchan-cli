package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseSleepWindow(t *testing.T) {
	for _, s := range []string{"", "off", "OFF", " off "} {
		if w, err := parseSleepWindow(s); w != nil || err != nil {
			t.Errorf("%q: got %v, %v; want nil, nil", s, w, err)
		}
	}
	w, err := parseSleepWindow("0:00-7:30")
	if err != nil || w.start != 0 || w.end != 7*60+30 {
		t.Errorf("0:00-7:30: got %+v, %v", w, err)
	}
	if got := w.String(); got != "00:00-07:30" {
		t.Errorf("String() = %q", got)
	}
	for _, s := range []string{"07:00", "7-8", "24:00-07:00", "00:60-07:00", "07:00-07:00", "a:00-07:00", "+1:00-07:00", "001:00-07:00", "1:0-07:00"} {
		if _, err := parseSleepWindow(s); err == nil {
			t.Errorf("%q: want an error", s)
		}
	}
}

func TestSleepWindowContains(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, time.Local) }
	tests := []struct {
		win  string
		h, m int
		want bool
	}{
		{"00:00-07:00", 0, 0, true},
		{"00:00-07:00", 6, 59, true},
		{"00:00-07:00", 7, 0, false}, // end is exclusive
		{"00:00-07:00", 23, 59, false},
		{"00:00-07:00", 12, 0, false},
		{"23:00-07:00", 23, 0, true}, // wraps past midnight
		{"23:00-07:00", 2, 0, true},
		{"23:00-07:00", 7, 0, false},
		{"23:00-07:00", 22, 59, false},
	}
	for _, tt := range tests {
		w, err := parseSleepWindow(tt.win)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.contains(at(tt.h, tt.m)); got != tt.want {
			t.Errorf("%s at %02d:%02d = %v, want %v", tt.win, tt.h, tt.m, got, tt.want)
		}
	}
	var none *sleepWindow
	if none.contains(at(3, 0)) {
		t.Error("a nil window contains 03:00")
	}
}

func TestSleepStateCommands(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, time.Local) }
	win, _ := parseSleepWindow("00:00-07:00")
	st := sleepState{win: win}

	if st.want(at(12, 0)) || !st.want(at(1, 0)) {
		t.Fatal("window alone: want awake at 12:00 and asleep at 01:00")
	}

	// sleep holds past the window's end until wakeup.
	st.sleep()
	for _, h := range []int{12, 3, 8} {
		if !st.want(at(h, 0)) {
			t.Errorf("after sleep: awake at %02d:00", h)
		}
	}
	st.wakeup(at(9, 0))
	if st.want(at(9, 0)) {
		t.Error("after wakeup at 09:00: still asleep")
	}
	if !st.want(at(1, 0)) {
		t.Error("after a daytime wakeup: the next night does not sleep")
	}

	// wakeup inside the window: awake until the window ends, then the next
	// night sleeps again.
	st.wakeup(at(2, 0))
	if st.want(at(2, 0)) || st.want(at(6, 59)) {
		t.Error("after wakeup at 02:00: asleep again before 07:00")
	}
	if st.want(at(7, 0)) {
		t.Error("asleep at 07:00")
	}
	if !st.want(at(0, 30)) {
		t.Error("after the window ended: the next night does not sleep")
	}

	// No window: only the commands.
	st = sleepState{}
	if st.want(at(3, 0)) {
		t.Error("no window: asleep at 03:00")
	}
	st.sleep()
	if !st.want(at(15, 0)) {
		t.Error("no window, after sleep: awake")
	}
	st.wakeup(at(15, 0))
	if st.want(at(15, 0)) {
		t.Error("no window, after wakeup: asleep")
	}
}

func TestResultError(t *testing.T) {
	tests := map[string]string{
		`{"error": "Timeout waiting for ESP32 response (method=x)"}`: "Timeout waiting for ESP32 response (method=x)",
		`{"ok": true, "error": null}`:                                "",
		`{"face":"happy","ok":true}`:                                 "",
		`true`:                                                       "",
		`not json`:                                                   "",
		`{"error": {"code": 1}}`:                                     "map[code:1]",
	}
	for text, want := range tests {
		if got := resultError(text); got != want {
			t.Errorf("resultError(%s) = %q, want %q", text, got, want)
		}
	}
}

func TestSleepWithoutDaemon(t *testing.T) {
	var out strings.Builder
	e := cmdEnv{out: &out, errOut: &out}
	if err := cmdSleep(nil, e); err == nil {
		t.Error("sleep without a daemon: want an error")
	}
	if err := cmdWakeup(e); err == nil {
		t.Error("wakeup without a daemon: want an error")
	}
	if err := cmdSleep([]string{"1"}, e); err != nil {
		t.Errorf("sleep 1 (pause): %v", err)
	}
}
