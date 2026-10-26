package main

// Sleep: while asleep the daemon turns the head-touch sensor off (stopping
// both the firmware's own touch motion and the touch events) and runs
// reactions/sleep.txt (screen dark by default). On waking it turns the sensor
// back on and runs reactions/wake.txt. StackChan sleeps during a daily window
// (daemon --sleep), and on demand: `sleep` until `wakeup` or the window's next
// end. Each change of state greets (reactions/goodnight.txt, goodmorning.txt).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stackchan-cli/internal/mcp"
)

const defaultSleepWindow = "00:00-07:00"

// sleepWindow is a daily [start, end) range in minutes since midnight. It
// wraps past midnight when start > end (e.g. 23:00-07:00).
type sleepWindow struct{ start, end int }

// parseSleepWindow parses "HH:MM-HH:MM". "" or "off" returns nil (no window).
func parseSleepWindow(s string) (*sleepWindow, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "off") {
		return nil, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return nil, fmt.Errorf("sleep window %q: want HH:MM-HH:MM", s)
	}
	start, err := parseClock(from)
	if err != nil {
		return nil, fmt.Errorf("sleep window %q: %w", s, err)
	}
	end, err := parseClock(to)
	if err != nil {
		return nil, fmt.Errorf("sleep window %q: %w", s, err)
	}
	if start == end {
		return nil, fmt.Errorf("sleep window %q: start and end are the same", s)
	}
	return &sleepWindow{start, end}, nil
}

// parseClock parses "H:MM" or "HH:MM" (digits only) into minutes since midnight.
func parseClock(s string) (int, error) {
	bad := fmt.Errorf("bad time %q (want HH:MM, 00:00-23:59)", s)
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || !isDigits(h) || !isDigits(m) || len(h) > 2 || len(m) != 2 {
		return 0, bad
	}
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	if hh > 23 || mm > 59 {
		return 0, bad
	}
	return hh*60 + mm, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (w *sleepWindow) contains(t time.Time) bool {
	if w == nil {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return w.start <= m && m < w.end
	}
	return m >= w.start || m < w.end
}

func (w *sleepWindow) String() string {
	if w == nil {
		return "off"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}

// nextEnd returns the first end of the window after t, or the zero time for
// no window.
func (w *sleepWindow) nextEnd(t time.Time) time.Time {
	if w == nil {
		return time.Time{}
	}
	end := time.Date(t.Year(), t.Month(), t.Day(), w.end/60, w.end%60, 0, 0, t.Location())
	if !end.After(t) {
		end = end.AddDate(0, 0, 1)
	}
	return end
}

// sleepState decides whether StackChan should be asleep, from the window and
// the sleep / wakeup commands.
type sleepState struct {
	win          *sleepWindow
	manual       bool      // `sleep`: asleep until `wakeup` or manualUntil
	manualUntil  time.Time // the window's next end after `sleep` (zero: no window, until `wakeup`)
	wokeInWindow bool      // `wakeup` inside the window: awake until the window ends
}

func (st *sleepState) want(now time.Time) bool {
	in := st.win.contains(now)
	if !in {
		st.wokeInWindow = false
	}
	if st.manual && !st.manualUntil.IsZero() && !now.Before(st.manualUntil) {
		st.manual = false
	}
	return st.manual || (in && !st.wokeInWindow)
}

// sleep returns when StackChan wakes by itself (zero: only on `wakeup`).
func (st *sleepState) sleep(now time.Time) time.Time {
	st.manual = true
	st.manualUntil = st.win.nextEnd(now)
	return st.manualUntil
}

func (st *sleepState) wakeup(now time.Time) {
	st.manual, st.manualUntil = false, time.Time{}
	st.wokeInWindow = st.win.contains(now)
}

// sleeper keeps the device's sleep state in line with sleepState.
type sleeper struct {
	c         *mcp.Client
	avatar    bool       // send the avatar set for the state (daemon --avatar)
	mu        sync.Mutex // guards st
	st        sleepState
	asleep    atomic.Bool   // the state last decided by run
	poke      chan struct{} // sleep / wakeup: check now
	reconnect chan struct{} // the device (re)connected: apply the state again
	// stepMu serializes touch reactions with applying the state, so a
	// reaction running when sleep starts finishes before sleep.txt, and none
	// starts after it.
	stepMu sync.Mutex
}

// daemonSleeper is set by the daemon before it serves commands; nil elsewhere.
var daemonSleeper *sleeper

// newSleeper decides the initial state at once, so the goroutines started
// after it (session watch, touch reactions) never see a stale "awake".
func newSleeper(win *sleepWindow, c *mcp.Client, avatar bool) *sleeper {
	s := &sleeper{
		c:         c,
		avatar:    avatar,
		st:        sleepState{win: win},
		poke:      make(chan struct{}, 1),
		reconnect: make(chan struct{}, 1),
	}
	s.asleep.Store(s.want())
	return s
}

func signal1(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// deviceConnected asks run to apply the current state to a new session.
func (s *sleeper) deviceConnected() { signal1(s.reconnect) }

// sleepNow and wakeNow are the sleep / wakeup commands. sleepNow returns when
// StackChan wakes by itself (zero: only on wakeup).
func (s *sleeper) sleepNow() time.Time {
	s.mu.Lock()
	until := s.st.sleep(time.Now())
	s.mu.Unlock()
	signal1(s.poke)
	return until
}

func (s *sleeper) wakeNow() {
	s.mu.Lock()
	s.st.wakeup(time.Now())
	s.mu.Unlock()
	signal1(s.poke)
}

func (s *sleeper) want() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.want(time.Now())
}

const sleepScriptAttempts = 5

// sleepSteps is what run still has to send to the device.
type sleepSteps struct {
	avatar   bool // send the avatar set for the state (tried once)
	sensor   bool // set the touch sensor (retried until it succeeds)
	script   bool // run sleep.txt / wake.txt (retried up to sleepScriptAttempts)
	face     bool // awake reconnect without wake.txt: just restore restFace
	attempts int  // script attempts so far
	// greetAt is when the state changed: say goodnight.txt / goodmorning.txt
	// once (zero: no greeting). Not retried, and dropped when the device
	// comes back later than greetLate.
	greetAt time.Time
}

const greetLate = 5 * time.Minute

// run checks the state every 30 s and on sleep / wakeup. On a change it sets
// the touch sensor and runs sleep.txt or wake.txt. The device often times
// out, so a failed step is tried again every 30 s while the device is
// connected; nothing is tried while it is away, as a reconnect applies the
// state anyway. Only a change of state greets (goodnight.txt /
// goodmorning.txt); a reconnect, the daemon starting, or a change noticed
// after a long gap between checks (the PC was suspended) does not. On a
// reconnect while awake only the sensor and the face are set, so a
// brightness changed by hand survives Wi-Fi drops; the first
// connection after the daemon starts runs wake.txt too (the daemon may have
// stopped while asleep). Every new session and every change of state also
// gets the avatar set for the state (the set lives in device RAM).
func (s *sleeper) run(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var todo sleepSteps
	if s.asleep.Load() {
		fmt.Fprintln(os.Stderr, "daemon: starting asleep")
		todo = sleepSteps{sensor: true, script: true}
	}
	firstConnect := true
	lastCheck := time.Now()
	for {
		want := s.want()
		now := time.Now()
		if want != s.asleep.Load() {
			s.asleep.Store(want)
			todo = sleepSteps{avatar: true, sensor: true, script: true, greetAt: now}
			if want {
				fmt.Fprintln(os.Stderr, "daemon: going to sleep")
			} else {
				fmt.Fprintln(os.Stderr, "daemon: waking up")
			}
			if gap := now.Sub(lastCheck); gap > greetLate {
				todo.greetAt = time.Time{}
				fmt.Fprintf(os.Stderr, "daemon: no greeting (the last check was %s ago)\n", gap.Round(time.Second))
			}
		}
		lastCheck = now
		if (todo.avatar || todo.sensor || todo.script || todo.face) && deviceIsConnected(s.c) {
			if todo.avatar {
				todo.avatar = false
				s.loadAvatar(want)
			}
			s.apply(&todo, want)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		case <-s.poke:
		case <-s.reconnect:
			todo.avatar, todo.sensor, todo.attempts = true, true, 0
			if s.asleep.Load() || firstConnect {
				todo.script = true
			} else if !todo.script {
				todo.face = true
			}
			firstConnect = false
		}
	}
}

// loadAvatar sends the sleep set (~/.stackchan/avatar-sleep) while asleep, or
// the default set. It is not retried: the firmware sometimes shows the new
// face but never sends its "loaded" reply, so a timeout is not conclusive.
// It runs outside stepMu, so touch reactions go on during the upload.
func (s *sleeper) loadAvatar(asleep bool) {
	if !s.avatar {
		return
	}
	set := ""
	if asleep {
		if _, err := os.Stat(avatarDir("sleep")); err == nil {
			set = "sleep"
		} else {
			fmt.Fprintf(os.Stderr, "daemon: no sleep avatar set (%s); make it with scripts/gen_avatars.py --sleep\n", avatarDir("sleep"))
		}
	}
	if text, err := loadAvatarQuiet(s.c, set); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: avatar load %q: %v\n", set, err)
	} else {
		fmt.Fprintf(os.Stderr, "daemon: avatar loaded %q %s\n", set, text)
	}
}

// apply sends the steps in todo and clears the ones that succeeded.
func (s *sleeper) apply(todo *sleepSteps, asleep bool) {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	if todo.sensor {
		todo.sensor = !s.setTouchSensor(!asleep)
	}
	// Goodnight before sleep.txt darkens the screen, good morning once
	// wake.txt has brightened it (or given up retrying).
	if asleep {
		s.greet(todo, true)
	}
	if todo.script {
		name := "wake"
		if asleep {
			name = "sleep"
		}
		todo.attempts++
		if failed := runNamedScript(s.c, name, defaultSleepScripts, true, nil); failed == 0 {
			todo.script, todo.face = false, false
		} else if todo.attempts >= sleepScriptAttempts {
			fmt.Fprintf(os.Stderr, "daemon: %s.txt: gave up after %d attempts\n", name, todo.attempts)
			todo.script = false
		} else {
			fmt.Fprintf(os.Stderr, "daemon: %s.txt: %d line(s) failed; retrying in 30s\n", name, failed)
		}
	}
	if todo.face {
		_, err := callTool(s.c, "set_avatar", map[string]any{"face": restFace}, nil)
		todo.face = err != nil
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: set_avatar %s: %v\n", restFace, err)
		}
	}
	if !asleep && !todo.script {
		s.greet(todo, false)
	}
	if todo.sensor {
		fmt.Fprintln(os.Stderr, "daemon: touch sensor not set yet; retrying in 30s")
	}
}

// greet runs goodnight.txt or goodmorning.txt once for a change of state.
func (s *sleeper) greet(todo *sleepSteps, asleep bool) {
	if todo.greetAt.IsZero() {
		return
	}
	at := todo.greetAt
	todo.greetAt = time.Time{}
	name := "goodmorning"
	if asleep {
		name = "goodnight"
	}
	if late := time.Since(at); late > greetLate {
		fmt.Fprintf(os.Stderr, "daemon: %s.txt: skipped (the device came back %s late)\n", name, late.Round(time.Second))
		return
	}
	if failed := runNamedScript(s.c, name, defaultSleepScripts, true, nil); failed > 0 {
		fmt.Fprintf(os.Stderr, "daemon: %s.txt: %d line(s) failed (not retried)\n", name, failed)
	}
}

// serialized runs fn (a timer notice) never at the same time as apply or a
// touch reaction, asleep or not.
func (s *sleeper) serialized(fn func()) {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	fn()
}

// reacting runs fn (a touch reaction) unless StackChan is asleep, and never
// at the same time as apply. It reports whether fn ran.
func (s *sleeper) reacting(fn func()) (ran bool) {
	s.serialized(func() {
		if !s.asleep.Load() {
			fn()
			ran = true
		}
	})
	return ran
}

func deviceIsConnected(c *mcp.Client) bool {
	res, err := c.CallTool("get_status", map[string]any{})
	return err == nil && isConnected(res.Text())
}

// setTouchSensor reports whether the device accepted the change. The gateway
// returns device errors as a normal result, so the reply must say ok:true.
func (s *sleeper) setTouchSensor(enabled bool) bool {
	res, err := callTool(s.c, "set_touch_sensor_enabled", map[string]any{"enabled": enabled}, nil)
	if err == nil && !strings.Contains(strings.ReplaceAll(res.Text(), " ", ""), `"ok":true`) {
		err = fmt.Errorf("%s", res.Text())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: touch sensor %v: %v\n", enabled, err)
		return false
	}
	return true
}

// defaultSleepScripts seed reactions/sleep.txt, wake.txt and the greetings.
var defaultSleepScripts = map[string]string{
	"sleep": `# 眠るとき（daemon --sleep の時刻になったとき、sleep コマンド）と、
# 眠っている間に本体が繋ぎ直したときに実行する。
# 頭のタッチセンサーは daemon が止めるので、ここには書かない。
# 寝顔（~/.stackchan/avatar-sleep。scripts/gen_avatars.py --sleep で作る）は daemon が送る。
# 寝顔を出さず画面を消すなら brightness 0 にする。
clear-leds
brightness 10
`,
	"wake": `# 起きるとき（--sleep の時刻が終わったとき、wakeup コマンド）と、
# daemon の起動後に初めて本体が繋がったときに実行する。
brightness 100
avatar ` + restFace + `
`,
	"goodnight": `# 眠るときの挨拶。daemon --sleep の時刻になったときと sleep コマンドで、1 回だけ実行する
# （本体が繋ぎ直したときは実行しない）。sleep.txt の前に実行する。
say "おやすみ"
`,
	"goodmorning": `# 起きるときの挨拶。--sleep の時刻が終わったときと wakeup コマンドで、1 回だけ実行する
# （daemon の起動時や本体が繋ぎ直したときは実行しない）。wake.txt の後に実行する。
say "おはよう"
`,
}

// cmdSleep is `sleep <ms>` (pause, for scripts) or, without an argument,
// puts StackChan to sleep until `wakeup` or the end of the sleep window
// (needs the daemon).
func cmdSleep(args []string, e cmdEnv) error {
	if len(args) > 0 {
		return doSleep(args)
	}
	if daemonSleeper == nil {
		return fmt.Errorf("sleep (without ms) needs a running daemon")
	}
	if until := daemonSleeper.sleepNow(); until.IsZero() {
		fmt.Fprintln(e.out, "going to sleep (until wakeup)")
	} else {
		fmt.Fprintf(e.out, "going to sleep (until wakeup or %s)\n", until.Format("01-02 15:04"))
	}
	return nil
}

func cmdWakeup(e cmdEnv) error {
	if daemonSleeper == nil {
		return fmt.Errorf("wakeup needs a running daemon")
	}
	daemonSleeper.wakeNow()
	fmt.Fprintln(e.out, "waking up")
	return nil
}

// runNamedScript runs reactionsDir/<name>.txt, or defaults[name] when the file
// is missing, with output to the daemon log. vars (nil for none) fills the
// script's placeholders first. It returns the failed lines; with
// deviceErrors, a tool result carrying "error" (the gateway reports device
// timeouts that way) counts as a failed line too.
func runNamedScript(c *mcp.Client, name string, defaults map[string]string, deviceErrors bool, vars *strings.Replacer) (failed int) {
	e := logEnv(c)
	e.deviceErrors = deviceErrors
	script, ok := defaults[name]
	if reactionsDir != "" {
		file := filepath.Join(reactionsDir, name+".txt")
		data, err := os.ReadFile(file)
		switch {
		case err == nil:
			script, ok = string(data), true
		case !errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			return 1
		}
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "daemon: no script for %q\n", name)
		return 0
	}
	if vars != nil {
		script = vars.Replace(script)
	}
	return runScript(script, e)
}

// resultError returns the "error" a tool result carries, or "". The gateway
// returns device errors (timeouts, no device) as a normal result like this.
func resultError(text string) string {
	var r struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(text), &r) != nil || r.Error == nil {
		return ""
	}
	if s, ok := r.Error.(string); ok {
		return s
	}
	return fmt.Sprint(r.Error)
}
