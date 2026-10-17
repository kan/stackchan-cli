package main

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func touchAt(at time.Time, durationMs int) touchEvent {
	return touchEvent{EventType: "touch", DurationMs: durationMs, at: at}
}

func TestTouchJudgeClassifiesByDuration(t *testing.T) {
	tests := []struct {
		durationMs int
		want       string
	}{
		{0, "tap"},
		{400, "tap"}, // the firmware already calls this a stroke
		{1000, "tap"},
		{1001, "stroke"},
		{5000, "stroke"},
	}
	for _, tt := range tests {
		j := touchJudge{tapMax: time.Second}
		if got, ignored := j.judge(touchAt(t0, tt.durationMs), t0); got != tt.want {
			t.Errorf("%dms: gesture = %q (ignored %q), want %q", tt.durationMs, got, ignored, tt.want)
		}
	}
}

func TestTouchJudgeIgnoresSameContact(t *testing.T) {
	j := touchJudge{tapMax: time.Second}
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
	j := touchJudge{tapMax: time.Second}
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
