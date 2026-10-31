package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// speech lets the daemon stop a say partway. The gateway cannot cancel a
// say once called (it sends the whole utterance before it returns), so the
// daemon says the text one sentence at a time and checks between sentences.
// A sentence already started is said to its end. A raw "call say" is one
// gateway call and cannot be stopped.
type speech struct {
	mu     sync.Mutex
	epoch  uint64      // bumped by stop: says begun before it are stopped
	active []time.Time // when each say in progress began speaking
}

// daemonSpeech is set while the daemon runs. Without it (one-shot, REPL)
// nothing can stop a say, so the text is said in one call.
var daemonSpeech *speech

// speaking is a say that began speaking (holds audioMu, about to send its
// first sentence). Waiting for the device, audioMu or the weather is not
// speaking: a stop then does not cancel the say.
type speaking struct {
	epoch uint64
	began time.Time
}

// begin registers a say that starts speaking now; end must follow.
func (s *speech) begin() speaking {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.active = append(s.active, now)
	return speaking{s.epoch, now}
}

func (s *speech) end(h speaking) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.active, h.began); i >= 0 {
		s.active = slices.Delete(s.active, i, i+1)
	}
}

// stopped reports whether stop was called since h began.
func (s *speech) stopped(h speaking) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch != h.epoch
}

// stop stops the says that began speaking before at, at their next sentence
// break. It reports whether there was one: a touch stops speech only then,
// otherwise it gets its usual reaction. A touch that waited in the queue
// does not stop a say begun after it was made.
func (s *speech) stop(at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, began := range s.active {
		if began.Before(at) {
			s.epoch++
			return true
		}
	}
	return false
}

// stops counts the stops so far (0 without the daemon). One say speaks at a
// time, so a change across a say means that say was stopped.
func (s *speech) stops() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// stopSpeaking stops the daemon's speech begun before at, if any.
func stopSpeaking(at time.Time) bool { return daemonSpeech != nil && daemonSpeech.stop(at) }

// cmdStop stops what the daemon is saying at the end of the sentence.
func cmdStop(args []string, e cmdEnv) error {
	set, _ := fs("stop")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() > 0 {
		return fmt.Errorf("stop takes no arguments (pomodoro stop / timer cancel stop those)")
	}
	if daemonSpeech == nil {
		return fmt.Errorf("stop needs a running daemon")
	}
	if stopSpeaking(time.Now()) {
		fmt.Fprintln(e.out, "stopping speech (after the sentence being said)")
	} else {
		fmt.Fprintln(e.out, "nothing is being said")
	}
	return nil
}

// sentenceEnds end a sentence outside brackets; sentenceClosers that follow
// one stay with it (an unbalanced わかった。」). Ends inside 「」 and the like
// do not split: 「本当？」と聞いた。 is one sentence. "." is not an end: it is
// also the decimal point (3.5度).
const (
	sentenceEnds    = "。！？!?\n"
	bracketOpeners  = "「『（(【〈《"
	bracketClosers  = "」』）)】〉》"
	sentenceClosers = bracketClosers + "\"'"
)

// splitSentences splits text after each sentence end. Blank pieces are
// dropped, and newlines are not kept. Text with no end is one sentence.
func splitSentences(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	depth := 0
	ended := false // in the run of ends and closers after a sentence (本当？！)
	for _, r := range text {
		isEnd := strings.ContainsRune(sentenceEnds, r) && (depth == 0 || r == '\n')
		if ended && !isEnd && !strings.ContainsRune(sentenceClosers, r) {
			flush()
			ended = false
		}
		switch {
		case r == '\n':
			depth = 0 // a bracket left open does not swallow the next lines
		case strings.ContainsRune(bracketOpeners, r):
			depth++
		case strings.ContainsRune(bracketClosers, r) && depth > 0:
			depth--
		}
		ended = ended || isEnd
		if r != '\n' {
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
