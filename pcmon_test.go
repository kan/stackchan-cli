package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func floatp(v float64) *float64 { return &v }

func TestParsePCConfig(t *testing.T) {
	c, err := parsePCConfig(defaultPCMonitorFile)
	if err != nil {
		t.Fatal(err)
	}
	if def := defaultPCConfig(); c.CPU != def.CPU || c.Mem != def.Mem || c.DiskFree != def.DiskFree ||
		c.Busy != def.Busy || c.Interval != def.Interval || c.Quiet != nil || strings.Join(c.Drives, ",") != "C:" {
		t.Errorf("the seeded file = %+v, want the defaults %+v", c, def)
	}

	c, err = parsePCConfig("cpu = 80\nmem=85 # comment\ndisk_free = 0\nbusy = 50\ninterval = 30\nquiet = 22:00-08:00\ndrives = c, D:\\\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.CPU != 80 || c.Mem != 85 || c.DiskFree != 0 || c.Busy != 50 || c.Interval != 30*time.Minute ||
		c.Quiet.String() != "22:00-08:00" || strings.Join(c.Drives, ",") != "C:,D:" {
		t.Errorf("parsed = %+v", c)
	}

	// Lines left out keep the defaults.
	if c, err := parsePCConfig("cpu = 95\n"); err != nil || c.Mem != 90 || c.Interval != time.Hour {
		t.Errorf("partial file = %+v, %v", c, err)
	}

	for _, bad := range []string{
		"cpu = 0", "cpu = 101", "mem = x", "busy = 0", "disk_free = -1", "disk_free_gb = -1", "disk_free_gb = 10GB",
		"interval = -5", "interval = 1h",
		"quiet = 25:00-07:00", "drives = CD", "drives = ", "drives = C:,", "color = red", "cpu 90",
	} {
		if _, err := parsePCConfig(bad); err == nil {
			t.Errorf("parsePCConfig(%q) accepted it", bad)
		}
	}
}

func TestLoadPCConfigMissing(t *testing.T) {
	fakeHome(t)
	c, err := loadPCConfig()
	if err != nil || c.CPU != 90 {
		t.Errorf("no file = %+v, %v; want the defaults", c, err)
	}
	// A bad line gives the defaults, not a half-read file with cpu = 0.
	seedFile(pcConfigPath(), "mem = 50\ncpu = 95%\n")
	c, err = loadPCConfig()
	if err == nil || c.CPU != 90 || c.Mem != 90 {
		t.Errorf("bad file = %+v, %v; want the defaults and the error", c, err)
	}
}

func TestCPUUse(t *testing.T) {
	a := cpuSample{idle: 100, total: 1000}
	b := cpuSample{idle: 400, total: 2000} // 300 idle of 1000
	if v, ok := cpuUse(a, b); !ok || v != 70 {
		t.Errorf("cpuUse = %v, %v; want 70", v, ok)
	}
	if _, ok := cpuUse(b, b); ok {
		t.Error("no time passed: want not ok")
	}
	if _, ok := cpuUse(b, a); ok {
		t.Error("backwards: want not ok")
	}
}

func TestCPUHistoryAverage(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	h := &cpuHistory{}
	// One sample a minute at 0:00..6:00, the CPU 50% busy in each minute:
	// the sample at i minutes is idle 50i of total 100i.
	for i := range 7 {
		h.add(cpuSample{at: t0.Add(time.Duration(i) * time.Minute), idle: uint64(50 * i), total: uint64(100 * i)})
	}
	// At 6:20, after 20 s at 100%: idle 300 of total 650.
	now := cpuSample{at: t0.Add(6*time.Minute + 20*time.Second), idle: 300, total: 650}
	// 1 minute back is the sample at 5:00 (idle 250 of 500): 50 idle of 150.
	// The one at 6:00 is too young (20 s, under 1 minute minus 30 s of slack).
	if v, ok := h.average(now, time.Minute); !ok || int(v+0.5) != 67 {
		t.Errorf("1-minute average = %v, %v; want 67", v, ok)
	}
	// A ticker's "5 minutes ago" may be a few µs short of 5 minutes.
	almost := cpuSample{at: t0.Add(6*time.Minute - time.Microsecond), idle: 300, total: 600}
	if v, ok := h.average(almost, 5*time.Minute); !ok || v != 50 {
		t.Errorf("5-minute average a µs short = %v, %v; want 50 (from 1:00)", v, ok)
	}
	if _, ok := h.average(now, 10*time.Minute); ok {
		t.Error("no sample 10 minutes back: want not ok")
	}
	// The newest sample is long gone (the PC slept): not a 1-minute average.
	late := cpuSample{at: t0.Add(time.Hour), idle: 400, total: 800}
	if _, ok := h.average(late, time.Minute); ok {
		t.Error("stale samples: want not ok")
	}

	for i := 0; i < 20; i++ {
		h.add(cpuSample{at: t0.Add(time.Duration(10+i) * time.Minute)})
	}
	if len(h.samples) != cpuHistoryKeep {
		t.Errorf("kept %d samples, want %d", len(h.samples), cpuHistoryKeep)
	}
}

func TestSpeakBytes(t *testing.T) {
	const gb = 1 << 30
	tests := []struct {
		n    uint64
		want string
	}{
		{120 * gb, "120ギガバイト"},
		{120*gb + gb/3, "120ギガバイト"},
		{8*gb + gb/2, "8.5ギガバイト"},
		{1 * gb, "1ギガバイト"},
		{800 << 20, "800メガバイト"},
		{1229 * gb, "1.2テラバイト"},
		{2 << 40, "2テラバイト"},
		{0, "0メガバイト"},
	}
	for _, tt := range tests {
		if got := speakBytes(tt.n); got != tt.want {
			t.Errorf("speakBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func testStats(cpu, mem float64, disks ...diskStat) pcStats {
	return pcStats{CPU: floatp(cpu), Mem: floatp(mem), Disks: disks}
}

const gib1 = uint64(1 << 30)

func TestPCReport(t *testing.T) {
	c := defaultPCConfig()
	cDrive := diskStat{"C:", 120 * gib1, 476 * gib1}
	tests := []struct {
		name      string
		s         pcStats
		want, fce string
	}{
		{"all fine", testStats(12, 45, cDrive),
			"元気だよ。CPUは12パーセント、メモリは45パーセント、ディスクの空きは120ギガバイト。", "happy"},
		{"busy", testStats(12, 75, cDrive),
			"ちょっと忙しい。メモリが75パーセント。CPUは12パーセント、ディスクの空きは120ギガバイト。", "thinking"},
		{"over first, then busy", testStats(72, 92, cDrive),
			"ちょっと疲れてる。メモリが92パーセント、CPUが72パーセント。ディスクの空きは120ギガバイト。", "sad"},
		{"both busy: the fuller first", testStats(71, 80, cDrive),
			"ちょっと忙しい。メモリが80パーセント、CPUが71パーセント。ディスクの空きは120ギガバイト。", "thinking"},
		{"disk low", testStats(12, 45, diskStat{"C:", 8 * gib1, 476 * gib1}),
			"ちょっと疲れてる。ディスクの空きが8ギガバイトしかない。CPUは12パーセント、メモリは45パーセント。", "sad"},
		// 89.6 is said as 90, so it is judged as 90 too.
		{"rounded before judging", testStats(89.6, 45, cDrive),
			"ちょっと疲れてる。CPUが90パーセント。メモリは45パーセント、ディスクの空きは120ギガバイト。", "sad"},
		{"two drives are named", testStats(12, 45, cDrive, diskStat{"D:", 2 << 40, 4 << 40}),
			"元気だよ。CPUは12パーセント、メモリは45パーセント、Cドライブの空きは120ギガバイト、Dドライブの空きは2テラバイト。", "happy"},
		{"CPU unknown", pcStats{Mem: floatp(45)},
			"元気だよ。メモリは45パーセント。", "happy"},
		{"nothing read", pcStats{}, pcFailed, ""},
	}
	for _, tt := range tests {
		cfg := c
		for _, d := range tt.s.Disks[min(1, len(tt.s.Disks)):] {
			cfg.Drives = append(cfg.Drives, d.Drive)
		}
		text, face := pcReport(judgePC(tt.s, cfg))
		if text != tt.want || face != tt.fce {
			t.Errorf("%s:\n got %q, %q\nwant %q, %q", tt.name, text, face, tt.want, tt.fce)
		}
	}
}

func TestPCWarner(t *testing.T) {
	c := defaultPCConfig()
	disk := diskStat{"C:", 120 * gib1, 476 * gib1}
	w := pcWarner{}
	keys := func(items []pcItem) string {
		var k []string
		for _, it := range items {
			k = append(k, it.key)
		}
		return strings.Join(k, ",")
	}

	if due := w.due(judgePC(testStats(50, 50, disk), c)); len(due) != 0 {
		t.Errorf("all under: due %s", keys(due))
	}
	due := w.due(judgePC(testStats(95, 92, disk), c))
	if keys(due) != "cpu,mem" {
		t.Fatalf("due = %s, want cpu,mem", keys(due))
	}
	if got, want := pcWarningText(due), "CPUが95パーセントの状態が続いてるよ。重い処理が動いてないか見てみて。メモリが92パーセントだよ。何か閉じたほうがいいかも。"; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
	// Not said (asleep, no device): due again.
	if due := w.due(judgePC(testStats(95, 92, disk), c)); keys(due) != "cpu,mem" {
		t.Errorf("not said, then due = %s", keys(due))
	}
	w.done(due)
	// Said: quiet while still over.
	if due := w.due(judgePC(testStats(96, 93, disk), c)); len(due) != 0 {
		t.Errorf("still over: due %s", keys(due))
	}
	// Memory went under, CPU could not be read (left as warned).
	if due := w.due(judgePC(pcStats{Mem: floatp(60), Disks: []diskStat{disk}}, c)); len(due) != 0 {
		t.Errorf("under: due %s", keys(due))
	}
	if due := w.due(judgePC(testStats(97, 91, disk), c)); keys(due) != "mem" {
		t.Errorf("memory over again = %s, want mem only", keys(due))
	}

	// disk_free_gb replaces the percentage: 26 GB free (3%) is fine above 10 GB.
	gbCfg, err := parsePCConfig("disk_free_gb = 10\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		free uint64
		want pcLevel
	}{{26 * gib1, pcOK}, {10 * gib1, pcOK}, {10*gib1 - 1, pcOver}} {
		items := judgePC(pcStats{Disks: []diskStat{{"C:", tt.free, 952 * gib1}}}, gbCfg)
		if items[0].level != tt.want {
			t.Errorf("disk_free_gb = 10, %d bytes free: %s, want %s", tt.free, items[0].level, tt.want)
		}
	}

	low := diskStat{"C:", 8 * gib1, 476 * gib1}
	due = w.due(judgePC(testStats(10, 10, low), c))
	if got, want := pcWarningText(due), "ディスクの空きが8ギガバイトしかないよ。いらないファイルを消したほうがいいかも。"; got != want {
		t.Errorf("disk warning = %q, want %q", got, want)
	}
}

// expandPC reads the real values (Windows) or says it cannot (elsewhere).
func TestExpandPC(t *testing.T) {
	fakeHome(t)
	if got, face := expandPC("こんにちは", nil); got != "こんにちは" || face != "" {
		t.Errorf("no placeholder = %q, %q", got, face)
	}
	var notes bytes.Buffer
	got, face := expandPC("{cpu}/{mem}/{disk}", &notes)
	if face != "" {
		t.Errorf("face without {pc} = %q", face)
	}
	text, pcFace := expandPC("{pc}", &notes)
	if runtime.GOOS != "windows" {
		if got != "不明/不明/不明" || text != pcFailed || pcFace != "" {
			t.Errorf("not Windows: %q, %q, %q", got, text, pcFace)
		}
		return
	}
	parts := strings.Split(got, "/")
	if len(parts) != 3 || parts[0] == pcUnknown || parts[1] == pcUnknown || !strings.HasSuffix(parts[2], "バイト") {
		t.Errorf("Windows: %q (notes %q)", got, notes.String())
	}
	if !strings.Contains(text, "CPU") || pcFace == "" {
		t.Errorf("{pc} = %q, face %q (notes %q)", text, pcFace, notes.String())
	}
}

func TestReadPCStatsWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	s, err := readPCStats([]string{drive}, currentCPU)
	if err != nil {
		t.Fatal(err)
	}
	if *s.CPU < 0 || *s.CPU > 100 || *s.Mem <= 0 || *s.Mem > 100 {
		t.Errorf("CPU %v, memory %v", *s.CPU, *s.Mem)
	}
	if len(s.Disks) != 1 || s.Disks[0].Total == 0 || s.Disks[0].Free > s.Disks[0].Total {
		t.Errorf("disks %+v", s.Disks)
	}
	t.Logf("CPU %.1f%%, memory %.0f%%, %s %s free of %s", *s.CPU, *s.Mem, drive, gib(s.Disks[0].Free), gib(s.Disks[0].Total))
}
