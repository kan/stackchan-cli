package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitSentences(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"こんにちは", []string{"こんにちは"}},
		{"晴れだよ。気温は20度。", []string{"晴れだよ。", "気温は20度。"}},
		{"本当？！すごいね", []string{"本当？！", "すごいね"}},
		{"「本当？」と聞いた。次だよ。", []string{"「本当？」と聞いた。", "次だよ。"}},
		{"わかった。」次だよ", []string{"わかった。」", "次だよ"}},
		{"「閉じない。\n次の行", []string{"「閉じない。", "次の行"}},
		{"3.5度だよ。OK!", []string{"3.5度だよ。", "OK!"}},
		{"一行目\n\n二行目", []string{"一行目", "二行目"}},
		{"はい。\n いいえ。 ", []string{"はい。", "いいえ。"}},
		{"   ", nil},
		{"", nil},
	}
	for _, tt := range tests {
		if got := splitSentences(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("splitSentences(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSpeechStop(t *testing.T) {
	var s speech
	if s.stop(time.Now()) {
		t.Fatal("stop with nothing being said reported true")
	}
	before := time.Now().Add(-time.Second)
	h := s.begin()
	if s.stop(before) {
		t.Fatal("a touch made before the say began stopped it")
	}
	if s.stopped(h) {
		t.Fatal("stopped before stop")
	}
	if !s.stop(time.Now().Add(time.Millisecond)) {
		t.Fatal("stop while speaking reported false")
	}
	if !s.stopped(h) {
		t.Fatal("not stopped after stop")
	}
	s.end(h)
	// A say begun after the stop is not stopped by it.
	h2 := s.begin()
	if s.stopped(h2) {
		t.Fatal("a later say was stopped by an earlier stop")
	}
	s.end(h2)
	if s.stop(time.Now().Add(time.Millisecond)) {
		t.Fatal("stop after every say ended reported true")
	}
}

func TestSpeakSentencesStopsBetweenSentences(t *testing.T) {
	sp := &speech{}
	var said []string
	say := func(text string) error {
		said = append(said, text)
		if len(said) == 2 {
			sp.stop(time.Now().Add(time.Millisecond))
		}
		return nil
	}
	var out strings.Builder
	if err := speakSentences(sp, []string{"a。", "b。", "c。"}, say, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(said, []string{"a。", "b。"}) {
		t.Errorf("said %q, want the first two", said)
	}
	if !strings.Contains(out.String(), "stopped (2 of 3") {
		t.Errorf("output %q does not report the stop", out.String())
	}
	// audioMu is released, so the next say can run.
	if !audioMu.TryLock() {
		t.Fatal("audioMu still held after a stopped say")
	}
	audioMu.Unlock()
	if sp.stop(time.Now().Add(time.Millisecond)) {
		t.Fatal("the say still counts as speaking after it returned")
	}
}

func TestSpeakSentencesError(t *testing.T) {
	calls := 0
	boom := errors.New("boom")
	say := func(string) error { calls++; return boom }
	err := speakSentences(nil, []string{"a。", "b。"}, say, &strings.Builder{})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err=%v calls=%d, want boom after 1 call", err, calls)
	}
	// A device error in the result skips the rest without failing.
	calls = 0
	say = func(string) error { calls++; return errSayRest }
	if err := speakSentences(nil, []string{"a。", "b。"}, say, &strings.Builder{}); err != nil || calls != 1 {
		t.Errorf("err=%v calls=%d, want nil after 1 call", err, calls)
	}
}

func TestCmdStop(t *testing.T) {
	daemonSpeech = nil
	var out strings.Builder
	e := cmdEnv{out: &out, errOut: &out}
	if err := cmdStop(nil, e); err == nil {
		t.Fatal("stop without the daemon did not fail")
	}
	daemonSpeech = &speech{}
	t.Cleanup(func() { daemonSpeech = nil })
	if err := cmdStop([]string{"foo"}, e); err == nil {
		t.Fatal("stop with an argument did not fail")
	}
	if err := cmdStop(nil, e); err != nil || !strings.Contains(out.String(), "nothing is being said") {
		t.Errorf("err=%v out=%q", err, out.String())
	}
}
