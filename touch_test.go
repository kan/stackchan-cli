package main

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func touchAt(at time.Time, durationMs int) touchEvent {
	return touchEvent{EventType: "touch", Subtype: "tap", DurationMs: durationMs, at: at}
}

func TestTouchJudgeTakesFirmwareGesture(t *testing.T) {
	tests := []struct {
		subtype    string
		durationMs int
		want       string
		reacts     bool
	}{
		{"tap", 300, "tap", true},
		{"stroke", 600, "stroke", true}, // a quick stroke is under a second
		{"stroke", 3000, "stroke", true},
		{"STROKE", 300, "stroke", true},
		{"", 300, "", false}, // still answers a pomodoro, no reaction
		{"hold", 300, "hold", false},
	}
	for _, tt := range tests {
		var j touchJudge
		ev := touchAt(t0, tt.durationMs)
		ev.Subtype = tt.subtype
		got, ignored := j.judge(ev, t0)
		if got != tt.want || ignored != "" || reactsTo(got) != tt.reacts {
			t.Errorf("%q %dms: gesture = %q (ignored %q, reacts %v), want %q (reacts %v)",
				tt.subtype, tt.durationMs, got, ignored, reactsTo(got), tt.want, tt.reacts)
		}
	}
}

func TestTouchJudgeIgnoresSameContact(t *testing.T) {
	var j touchJudge
	steps := []struct {
		after  time.Duration // arrival, from t0
		answer bool
	}{
		{0, true},
		{500 * time.Millisecond, false}, // same contact as the touch at t0
		{999 * time.Millisecond, false},
		{time.Second, true}, // counted from the last answered touch (t0)
		{1500 * time.Millisecond, false},
		{2100 * time.Millisecond, true},
	}
	for _, s := range steps {
		at := t0.Add(s.after)
		gesture, ignored := j.judge(touchAt(at, 300), at)
		if answered := gesture != ""; answered != s.answer {
			t.Errorf("touch at +%s: answered = %v (ignored %q), want %v", s.after, answered, ignored, s.answer)
		}
	}
}

func TestTouchJudgeIgnoresStaleTouch(t *testing.T) {
	var j touchJudge
	if gesture, _ := j.judge(touchAt(t0, 300), t0.Add(touchMaxWait)); gesture == "" {
		t.Errorf("a touch that waited exactly %s was ignored, want answered", touchMaxWait)
	}
	at := t0.Add(5 * time.Second)
	gesture, ignored := j.judge(touchAt(at, 300), at.Add(touchMaxWait+time.Millisecond))
	if gesture != "" {
		t.Errorf("a touch that waited over %s was answered", touchMaxWait)
	}
	if !strings.Contains(ignored, "waited") {
		t.Errorf("ignored = %q", ignored)
	}
}

func TestQueueLatestKeepsOnlyNewest(t *testing.T) {
	q := make(chan touchEvent, 1)
	queueLatest(q, touchAt(t0, 100))
	if len(q) != 1 {
		t.Fatalf("queue length = %d after one touch, want 1", len(q))
	}
	queueLatest(q, touchAt(t0.Add(time.Second), 200))
	queueLatest(q, touchAt(t0.Add(2*time.Second), 300))
	if len(q) != 1 {
		t.Fatalf("queue length = %d, want 1", len(q))
	}
	if ev := <-q; ev.DurationMs != 300 {
		t.Errorf("queued touch = %dms, want the newest (300ms)", ev.DurationMs)
	}
}
