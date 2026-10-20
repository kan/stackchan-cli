package main

// PC monitor: say's {pc}, {cpu}, {mem} and {disk} read this PC's CPU,
// memory and disk space, and the daemon warns when one goes over its
// threshold. Settings are ~/.stackchan/pc-monitor.txt, read again at every
// use, so a change needs no restart. The values come from Windows APIs
// (pcstat_windows.go); elsewhere they cannot be read.

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"stackchan-cli/internal/mcp"
)

func pcConfigPath() string { return filepath.Join(stackchanDir(), "pc-monitor.txt") }

// pcConfig is ~/.stackchan/pc-monitor.txt.
type pcConfig struct {
	CPU        int           // warn at this CPU use (%, 5-minute average) or more
	Mem        int           // warn at this memory use (%) or more
	DiskFree   int           // warn below this much free space (% of the drive)
	DiskFreeGB int           // instead, when > 0: warn below this many GB (1024³ bytes) free
	Busy       int           // {pc} says "busy" from this CPU or memory use (%)
	Interval   time.Duration // between warning checks; 0 = never warn
	Quiet      *sleepWindow  // also silent in this daily window (always while asleep)
	Drives     []string      // "C:", "D:"
}

func defaultPCConfig() pcConfig {
	return pcConfig{CPU: 90, Mem: 90, DiskFree: 10, Busy: 70, Interval: time.Hour, Drives: []string{"C:"}}
}

// defaultPCMonitorFile seeds pc-monitor.txt; its values are defaultPCConfig.
const defaultPCMonitorFile = `# PC の見張り。say の {pc} {cpu} {mem} {disk} と、daemon の定期の警告が使う。
# 保存すれば次に使うときから反映される（再起動は要らない）。行を消すと初期値に戻る。
cpu = 90         # CPU の使用率（5 分の平均）がこの % 以上で警告する
mem = 90         # メモリの使用率がこの % 以上で警告する
disk_free = 10   # ディスクの空きがこの % 未満で警告する
disk_free_gb = 0 # 1 以上なら、% の代わりに空きがこの GB 未満で警告する
busy = 70        # 「調子どう」で、CPU かメモリがこの % 以上なら「ちょっと忙しい」と言う
interval = 60    # 警告の確認の間隔（分）。0 で警告しない
quiet = off      # 警告を話さない時間帯（HH:MM-HH:MM）。眠っている間はいつも話さない
drives = C:      # 見るドライブ（, 区切り。例: C:, D:）
`

func parsePCConfig(text string) (pcConfig, error) {
	c := defaultPCConfig()
	err := parseKeyValues("pc-monitor.txt", text, func(key, value string) error {
		var err error
		switch key {
		case "cpu":
			c.CPU, err = percentSetting(value, 1)
		case "mem":
			c.Mem, err = percentSetting(value, 1)
		case "disk_free":
			c.DiskFree, err = percentSetting(value, 0)
		case "disk_free_gb":
			gb, convErr := strconv.Atoi(value)
			if convErr != nil || gb < 0 {
				err = errors.New("want GB (0 = use disk_free)")
			}
			c.DiskFreeGB = gb
		case "busy":
			c.Busy, err = percentSetting(value, 1)
		case "interval":
			m, convErr := strconv.Atoi(value)
			if convErr != nil || m < 0 {
				err = errors.New("want minutes (0 = no warnings)")
			}
			c.Interval = time.Duration(m) * time.Minute
		case "quiet":
			c.Quiet, err = parseSleepWindow(value)
		case "drives":
			c.Drives, err = parseDrives(value)
		default:
			return fmt.Errorf("unknown key %q (keys: cpu mem disk_free disk_free_gb busy interval quiet drives)", key)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	})
	return c, err
}

func percentSetting(s string, lo int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > 100 {
		return 0, fmt.Errorf("want a percentage %d..100, got %q", lo, s)
	}
	return v, nil
}

// parseDrives reads "C:, d" as ["C:", "D:"].
func parseDrives(s string) ([]string, error) {
	var drives []string
	for d := range strings.SplitSeq(s, ",") {
		d = strings.ToUpper(strings.TrimRight(strings.TrimSpace(d), `:\`))
		if len(d) != 1 || d[0] < 'A' || d[0] > 'Z' {
			return nil, fmt.Errorf("want drive letters like C:, D:, got %q", s)
		}
		drives = append(drives, d+":")
	}
	return drives, nil
}

// loadPCConfig reads pc-monitor.txt; without it the defaults apply. With an
// error it returns the defaults too, not a half-read file (a bad threshold
// would be 0 and warn about everything).
func loadPCConfig() (pcConfig, error) {
	data, err := os.ReadFile(pcConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return defaultPCConfig(), nil
	}
	if err != nil {
		return defaultPCConfig(), err
	}
	c, err := parsePCConfig(string(data))
	if err != nil {
		return defaultPCConfig(), err
	}
	return c, nil
}

// cpuSample is the CPU time spent since boot (100-ns units), idle included.
type cpuSample struct {
	at          time.Time
	idle, total uint64
}

// cpuUse is the CPU use (%) between two samples.
func cpuUse(from, to cpuSample) (float64, bool) {
	if to.total <= from.total || to.idle < from.idle {
		return 0, false
	}
	busy := 1 - float64(to.idle-from.idle)/float64(to.total-from.total)
	return math.Max(0, math.Min(100, 100*busy)), true
}

// cpuSampleEvery is how often the daemon records the CPU time.
const cpuSampleEvery = time.Minute

// cpuHistory is the daemon's record of CPU time, one sample a minute, so
// the use over the last minutes is known at once. CPU use jumps from moment
// to moment; an average over minutes says more.
type cpuHistory struct {
	mu      sync.Mutex
	samples []cpuSample // oldest first
}

const cpuHistoryKeep = 10

func (h *cpuHistory) add(s cpuSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, s)
	if len(h.samples) > cpuHistoryKeep {
		h.samples = h.samples[len(h.samples)-cpuHistoryKeep:]
	}
}

// average is the CPU use from the newest sample taken about d before now
// up to now. A sample a little younger than d counts (up to half a sampling
// interval): the samples come from a ticker, so the one taken "5 minutes
// ago" may be a few µs short of it. It fails when there is no such sample,
// or when it is much older than d (the sampling stopped, e.g. while the PC
// slept).
func (h *cpuHistory) average(now cpuSample, d time.Duration) (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	slack := min(d/2, cpuSampleEvery/2)
	for i := len(h.samples) - 1; i >= 0; i-- {
		s := h.samples[i]
		if age := now.at.Sub(s.at); age >= d-slack {
			if age > d+2*cpuSampleEvery {
				return 0, false
			}
			return cpuUse(s, now)
		}
	}
	return 0, false
}

// daemonCPU is set by the daemon before it serves commands; nil elsewhere.
var daemonCPU *cpuHistory

// cpuAverage is the CPU use over about the last d, from the daemon's record.
func cpuAverage(d time.Duration) (float64, error) {
	now, err := sampleCPU()
	if err != nil {
		return 0, err
	}
	if daemonCPU != nil {
		if v, ok := daemonCPU.average(now, d); ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no %s average yet", d)
}

// currentCPU is the CPU use for {pc}: over the last minute or two when the
// daemon has a record, since its newest sample in its first minute, else
// measured over one second.
func currentCPU() (float64, error) {
	for _, d := range []time.Duration{time.Minute, time.Second} {
		if daemonCPU == nil {
			break
		}
		if v, err := cpuAverage(d); err == nil {
			return v, nil
		}
	}
	now, err := sampleCPU()
	if err != nil {
		return 0, err
	}
	time.Sleep(time.Second)
	later, err := sampleCPU()
	if err != nil {
		return 0, err
	}
	v, ok := cpuUse(now, later)
	if !ok {
		return 0, errors.New("CPU time did not advance")
	}
	return v, nil
}

type diskStat struct {
	Drive       string // "C:"
	Free, Total uint64 // bytes
}

// pcStats is what was read; a value that could not be read is nil or missing.
type pcStats struct {
	CPU   *float64
	Mem   *float64
	Disks []diskStat
}

// readPCStats reads the values; the ones that fail are left out and their
// errors returned together.
func readPCStats(drives []string, cpu func() (float64, error)) (pcStats, error) {
	var s pcStats
	var errs []error
	if v, err := cpu(); err == nil {
		s.CPU = &v
	} else {
		errs = append(errs, fmt.Errorf("CPU: %w", err))
	}
	if v, err := memoryLoad(); err == nil {
		s.Mem = &v
	} else {
		errs = append(errs, fmt.Errorf("memory: %w", err))
	}
	for _, d := range drives {
		free, total, err := diskSpace(d)
		if err != nil {
			errs = append(errs, fmt.Errorf("disk: %w", err))
			continue
		}
		s.Disks = append(s.Disks, diskStat{d, free, total})
	}
	return s, errors.Join(errs...)
}

// speakBytes reads a size the way Explorer shows it (1 GB = 1024³ bytes):
// "120ギガバイト", "8.5ギガバイト", "1.2テラバイト", "800メガバイト".
func speakBytes(n uint64) string {
	const mb, gb, tb = 1 << 20, 1 << 30, 1 << 40
	oneDecimal := func(v float64) string {
		return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
	}
	switch {
	case n >= tb:
		return oneDecimal(float64(n)/tb) + "テラバイト"
	case n >= 10*gb:
		return strconv.Itoa(int(math.Round(float64(n)/gb))) + "ギガバイト"
	case n >= gb:
		return oneDecimal(float64(n)/gb) + "ギガバイト"
	}
	return strconv.Itoa(int(math.Round(float64(n)/mb))) + "メガバイト"
}

type pcLevel int

const (
	pcOK   pcLevel = iota
	pcBusy         // CPU or memory at busy or more ({pc} only)
	pcOver         // over the warning threshold
)

func (l pcLevel) String() string {
	return [...]string{"ok", "busy", "over"}[l]
}

// pcItem is one value as it is read out and judged.
type pcItem struct {
	key    string  // cpu, mem, disk C: (the warner remembers warnings by it)
	label  string  // CPU / メモリ / ディスク / Cドライブ (when there are several)
	number string  // the value for {cpu} {mem} {disk}: "12", "120ギガバイト"
	detail string  // for the pc command and the log
	level  pcLevel //
	load   float64 // how full (%), to say the worst first
	isDisk bool
}

// name is what the value is called: "メモリ", "ディスクの空き".
func (it pcItem) name() string {
	if it.isDisk {
		return it.label + "の空き"
	}
	return it.label
}

// value is the value as {pc} says it: "12パーセント", "120ギガバイト".
func (it pcItem) value() string {
	if it.isDisk {
		return it.number
	}
	return it.number + "パーセント"
}

// warning is what the daemon says when the value is over its threshold.
func (it pcItem) warning() string {
	switch {
	case it.isDisk:
		return it.name() + "が" + it.value() + "しかないよ。いらないファイルを消したほうがいいかも。"
	case it.key == "cpu":
		return it.name() + "が" + it.value() + "の状態が続いてるよ。重い処理が動いてないか見てみて。"
	}
	return it.name() + "が" + it.value() + "だよ。何か閉じたほうがいいかも。"
}

// judgePC judges each value against c. Percentages are rounded first, so
// what is said and what is judged agree.
func judgePC(s pcStats, c pcConfig) []pcItem {
	var items []pcItem
	usage := func(key, label string, v *float64, over int) {
		if v == nil {
			return
		}
		p := int(math.Round(*v))
		it := pcItem{key: key, label: label, number: strconv.Itoa(p), detail: fmt.Sprintf("%d%%", p), load: float64(p)}
		switch {
		case p >= over:
			it.level = pcOver
		case p >= c.Busy:
			it.level = pcBusy
		}
		items = append(items, it)
	}
	usage("cpu", "CPU", s.CPU, c.CPU)
	usage("mem", "メモリ", s.Mem, c.Mem)
	for _, d := range s.Disks {
		if d.Total == 0 {
			continue
		}
		freePct := 100 * float64(d.Free) / float64(d.Total)
		label := "ディスク"
		if len(c.Drives) > 1 {
			label = strings.TrimSuffix(d.Drive, ":") + "ドライブ"
		}
		it := pcItem{
			key: "disk " + d.Drive, label: label, number: speakBytes(d.Free),
			detail: fmt.Sprintf("%s free of %s (%.0f%% free)", gib(d.Free), gib(d.Total), freePct),
			load:   100 - freePct, isDisk: true,
		}
		over := math.Round(freePct) < float64(c.DiskFree) // as detail shows it
		if c.DiskFreeGB > 0 {
			over = d.Free < uint64(c.DiskFreeGB)<<30
		}
		if over {
			it.level = pcOver
		}
		items = append(items, it)
	}
	return items
}

// readPCItems reads and judges the values. Values that cannot be read are
// left out and reported to warn after prefix.
func readPCItems(c pcConfig, cpu func() (float64, error), warn io.Writer, prefix string) []pcItem {
	s, err := readPCStats(c.Drives, cpu)
	if err != nil {
		fmt.Fprintf(warn, "%s%v\n", prefix, err)
	}
	return judgePC(s, c)
}

func gib(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// pcMoods is how {pc} starts, and the face it shows while speaking, by the
// worst level.
var pcMoods = [...]struct{ text, face string }{
	pcOK:   {"元気だよ。", "happy"},
	pcBusy: {"ちょっと忙しい。", "thinking"},
	pcOver: {"ちょっと疲れてる。", "sad"},
}

// pcFailed is said for {pc} when nothing could be read; nothing is guessed.
const pcFailed = "ごめんね、パソコンの様子が分からなかった。"

// pcReport is {pc}: the mood, then the values that are busy or over (worst
// first), then the rest. "ちょっと疲れてる。メモリが92パーセント。CPUは12
// パーセント、ディスクの空きは120ギガバイト。" face is the one to show while
// speaking ("" when nothing could be read).
func pcReport(items []pcItem) (text, face string) {
	if len(items) == 0 {
		return pcFailed, ""
	}
	var worse, rest []string
	sorted := append([]pcItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].level != sorted[j].level {
			return sorted[i].level > sorted[j].level
		}
		return sorted[i].level > pcOK && sorted[i].load > sorted[j].load
	})
	top := sorted[0].level
	for _, it := range sorted {
		if it.level == pcOK {
			rest = append(rest, it.name()+"は"+it.value())
			continue
		}
		phrase := it.name() + "が" + it.value()
		if it.isDisk {
			phrase += "しかない"
		}
		worse = append(worse, phrase)
	}
	text = pcMoods[top].text
	if len(worse) > 0 {
		text += strings.Join(worse, "、") + "。"
	}
	if len(rest) > 0 {
		text += strings.Join(rest, "、") + "。"
	}
	return text, pcMoods[top].face
}

// pcUnknown fills {cpu} {mem} {disk} for a value that could not be read.
const pcUnknown = "不明"

var pcPlaceholders = []string{"{pc}", "{cpu}", "{mem}", "{disk}"}

// expandPC fills {pc}, {cpu}, {mem} and {disk}. face is the one to show
// while speaking when the text has {pc}, else "". Notes go to warn.
func expandPC(text string, warn io.Writer) (filled, face string) {
	if !slices.ContainsFunc(pcPlaceholders, func(p string) bool { return strings.Contains(text, p) }) {
		return text, ""
	}
	c, err := loadPCConfig()
	if err != nil {
		fmt.Fprintf(warn, "warning: %v (using the defaults)\n", err)
	}
	items := readPCItems(c, currentCPU, warn, "warning: pc: ")
	report, face := pcReport(items)
	if !strings.Contains(text, "{pc}") {
		face = ""
	}
	cpu, mem := pcUnknown, pcUnknown
	var disks []string
	for _, it := range items {
		switch {
		case it.key == "cpu":
			cpu = it.number
		case it.key == "mem":
			mem = it.number
		case it.isDisk && len(c.Drives) > 1:
			disks = append(disks, it.label+"が"+it.number)
		case it.isDisk:
			disks = append(disks, it.number)
		}
	}
	disk := pcUnknown
	if len(disks) > 0 {
		disk = strings.Join(disks, "、")
	}
	return strings.NewReplacer("{pc}", report, "{cpu}", cpu, "{mem}", mem, "{disk}", disk).Replace(text), face
}

// pcWarner remembers what was warned about (by item key), so a value that
// stays over its threshold is warned about once, and again only after it
// went back under.
type pcWarner map[string]bool

// due returns the items to warn about now. An item under its threshold is
// forgotten; one that could not be read is left as it was.
func (w pcWarner) due(items []pcItem) []pcItem {
	var out []pcItem
	for _, it := range items {
		switch {
		case it.level < pcOver:
			delete(w, it.key)
		case !w[it.key]:
			out = append(out, it)
		}
	}
	return out
}

// done records that items were warned about.
func (w pcWarner) done(items []pcItem) {
	for _, it := range items {
		w[it.key] = true
	}
}

func pcWarningText(items []pcItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.warning())
	}
	return b.String()
}

// pcCPUWindow is what the warnings average the CPU over. The daemon's
// first check waits as long, so it has that average.
const pcCPUWindow = 5 * time.Minute

// watchPC records the CPU time every minute (for {pc} and the warnings) and,
// every interval of pc-monitor.txt, warns about the values over their
// threshold.
func watchPC(c *mcp.Client, sl *sleeper, stop <-chan struct{}) {
	if _, err := sampleCPU(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: PC monitor off: %v\n", err)
		return
	}
	t := time.NewTicker(cpuSampleEvery)
	defer t.Stop()
	w := pcWarner{}
	firstAt := time.Now().Add(pcCPUWindow)
	var lastCheck time.Time
	lastErr := "" // logged once until it changes
	for {
		if s, err := sampleCPU(); err == nil {
			daemonCPU.add(s)
		}
		cfg, err := loadPCConfig()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != "" && msg != lastErr {
			fmt.Fprintf(os.Stderr, "daemon: %s (using the defaults)\n", msg)
		}
		lastErr = msg
		if now := time.Now(); cfg.Interval > 0 && !now.Before(firstAt) && now.Sub(lastCheck) >= cfg.Interval {
			lastCheck = now
			pcCheck(c, sl, cfg, w)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// pcCheck reads the values, CPU averaged over 5 minutes, and says the
// warnings that are due. A warning not said (quiet hours, asleep, no device,
// a failed say) stays due for the next check.
func pcCheck(c *mcp.Client, sl *sleeper, cfg pcConfig, w pcWarner) {
	cpu5 := func() (float64, error) { return cpuAverage(pcCPUWindow) }
	items := readPCItems(cfg, cpu5, os.Stderr, "daemon: pc check: ")
	var summary []string
	for _, it := range items {
		summary = append(summary, fmt.Sprintf("%s %s (%s)", it.key, it.detail, it.level))
	}
	fmt.Fprintf(os.Stderr, "daemon: pc check: %s\n", strings.Join(summary, ", "))
	due := w.due(items)
	if len(due) == 0 {
		return
	}
	text := pcWarningText(due)
	switch {
	case cfg.Quiet.contains(time.Now()):
		fmt.Fprintf(os.Stderr, "daemon: pc warning held (quiet hours %s): %s\n", cfg.Quiet, text)
		return
	case !deviceIsConnected(c):
		fmt.Fprintf(os.Stderr, "daemon: pc warning held (no device): %s\n", text)
		return
	}
	var sayErr error
	spoke := sl.reacting(func() {
		fmt.Fprintf(os.Stderr, "daemon: pc warning: %s\n", text)
		// deviceErrors: a device timeout reported as a result is not "said".
		e := logEnv(c)
		e.deviceErrors = true
		sayErr = cmdSay([]string{"--while-face", "sad", text}, e)
	})
	switch {
	case !spoke:
		fmt.Fprintf(os.Stderr, "daemon: pc warning held (asleep): %s\n", text)
	case sayErr != nil:
		fmt.Fprintf(os.Stderr, "daemon: pc warning not said: %v\n", sayErr)
	default:
		w.done(due)
	}
}

// cmdPC shows the values and what {pc} would say, without speaking.
func cmdPC(args []string, e cmdEnv) error {
	set, _ := fs("pc")
	if err := set.Parse(args); err != nil {
		return err
	}
	c, err := loadPCConfig()
	if err != nil {
		return err
	}
	items := readPCItems(c, currentCPU, e.errOut, "warning: ")
	for _, it := range items {
		fmt.Fprintf(e.out, "%-8s %s (%s)\n", it.key, it.detail, it.level)
	}
	text, face := pcReport(items)
	fmt.Fprintf(e.out, "{pc}: %s\n", text)
	if face != "" {
		fmt.Fprintf(e.out, "face: %s\n", face)
	}
	return nil
}
