package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestGatewayArgs(t *testing.T) {
	t.Setenv("STACKCHAN_MDNS", "")
	if got := gatewayArgs(); !reflect.DeepEqual(got, []string{"serve", "--no-mdns"}) {
		t.Errorf("default = %v, want mDNS off", got)
	}
	t.Setenv("STACKCHAN_MDNS", "1")
	if got := gatewayArgs(); !reflect.DeepEqual(got, []string{"serve"}) {
		t.Errorf("STACKCHAN_MDNS=1: %v, want mDNS on", got)
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{`status`, []string{"status"}},
		{`move-head  --yaw 30	--pitch 45`, []string{"move-head", "--yaw", "30", "--pitch", "45"}},
		{`say "hello world"`, []string{"say", "hello world"}},
		{`call t --json '{"a":"b c"}'`, []string{"call", "t", "--json", `{"a":"b c"}`}},
		{`say "it's"`, []string{"say", "it's"}},
		{`x "" ''`, []string{"x", "", ""}},
		{`--json='{"a":1}'`, []string{`--json={"a":1}`}},
		{`say "なでられるの 好き"`, []string{"say", "なでられるの 好き"}},
		{``, nil},
		{`   `, nil},
	}
	for _, tt := range tests {
		if got := tokenize(tt.line); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("tokenize(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

// The daemon receives quoteArgs(argv) and splits it with tokenize, so the
// round trip must give back argv.
func TestQuoteArgsRoundTrip(t *testing.T) {
	tests := [][]string{
		{"status"},
		{"say", "hello world"},
		{"say", "tab\tinside"},
		{"call", "t", "--json", `{"a":"b c"}`},
		{"call", "t", "--json", `{"a":1}`},
		{"say", "it's"},
		{"say", ""},
		{"say", "--face", "", "なでられるの 好き"},
	}
	for _, argv := range tests {
		line := quoteArgs(argv)
		if got := tokenize(line); !reflect.DeepEqual(got, argv) {
			t.Errorf("tokenize(quoteArgs(%q)) = %q (line %q)", argv, got, line)
		}
	}
}

// Known limitation: the tokenizer has no escapes, so an argument with both
// quote characters cannot survive the round trip. If this test starts failing,
// the limitation is gone: update the test and the quoteArgs comment.
func TestQuoteArgsCannotCarryBothQuotes(t *testing.T) {
	argv := []string{"say", `it's "fine"`}
	if got := tokenize(quoteArgs(argv)); reflect.DeepEqual(got, argv) {
		t.Errorf("round trip of %q now works; the documented limitation is gone", argv)
	}
}

func TestAbsSourcePath(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	already := t.TempDir()
	tests := []struct {
		argv []string
		want []string
	}{
		{[]string{"source", "rel/path.txt"}, []string{"source", abs("rel/path.txt")}},
		{[]string{"source", "--verbose", "rel/path.txt"}, []string{"source", "--verbose", abs("rel/path.txt")}},
		{[]string{"run", "x.txt"}, []string{"run", abs("x.txt")}},
		{[]string{"source", already}, []string{"source", already}},
		{[]string{"source"}, []string{"source"}},
		{[]string{"say", "rel/path.txt"}, []string{"say", "rel/path.txt"}},
	}
	for _, tt := range tests {
		in := append([]string(nil), tt.argv...)
		if got := absSourcePath(tt.argv); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("absSourcePath(%q) = %q, want %q", in, got, tt.want)
		}
		if !reflect.DeepEqual(tt.argv, in) {
			t.Errorf("absSourcePath modified its input: %q -> %q", in, tt.argv)
		}
	}
}

// The usage writes `call <tool> --json '{...}'`; the flags must be read
// there as well as before the name.
func TestParseCallArgs(t *testing.T) {
	tests := []struct {
		args    []string
		tool    string
		want    map[string]any
		verbose bool
	}{
		{[]string{"set_off_timeout", "--json", `{"seconds":0}`}, "set_off_timeout", map[string]any{"seconds": 0.0}, false},
		{[]string{"--json", `{"seconds":0}`, "set_off_timeout"}, "set_off_timeout", map[string]any{"seconds": 0.0}, false},
		{[]string{"get_status"}, "get_status", map[string]any{}, false},
		{[]string{"get_status", "--verbose"}, "get_status", map[string]any{}, true},
		{[]string{"--verbose", "set_off_timeout", "--json", `{"seconds":0}`}, "set_off_timeout", map[string]any{"seconds": 0.0}, true},
		{[]string{"--json", `{"seconds":0}`, "set_off_timeout", "--verbose"}, "set_off_timeout", map[string]any{"seconds": 0.0}, true},
	}
	for _, tt := range tests {
		tool, got, verbose, err := parseCallArgs(tt.args)
		if err != nil || tool != tt.tool || !reflect.DeepEqual(got, tt.want) || verbose != tt.verbose {
			t.Errorf("parseCallArgs(%q) = %q, %v, %v, %v", tt.args, tool, got, verbose, err)
		}
	}
	for _, args := range [][]string{
		{},
		{"--json", "{}"},
		{"t", "--json", "not json"},
		{"t", "extra"},
		{"--verbose", "t", "extra"},
	} {
		if _, _, _, err := parseCallArgs(args); err == nil {
			t.Errorf("parseCallArgs(%q) accepted it", args)
		}
	}
}

// After "--" everything is positional, even what looks like a flag.
func TestParseInterspersedStopsAtDashDash(t *testing.T) {
	set, _ := fs("t")
	quiet := set.Bool("quiet", false, "")
	pos, err := parseInterspersed(set, []string{"3m", "--", "--quiet", "x"})
	if err != nil || !reflect.DeepEqual(pos, []string{"3m", "--quiet", "x"}) || *quiet {
		t.Errorf("pos = %q, quiet %v, %v", pos, *quiet, err)
	}
	set, _ = fs("t")
	quiet = set.Bool("quiet", false, "")
	pos, err = parseInterspersed(set, []string{"3m", "--quiet", "5m"})
	if err != nil || !reflect.DeepEqual(pos, []string{"3m", "5m"}) || !*quiet {
		t.Errorf("pos = %q, quiet %v, %v", pos, *quiet, err)
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		text          string
		wantConnected bool
		wantSession   string
	}{
		{`{"connected": true, "device_id": "aa:bb", "session_id": "s1"}`, true, "s1"},
		{`{"connected": true, "device_id": "aa:bb"}`, true, ""},
		{`{"connected": false, "device_id": null, "tools_count": 0}`, false, ""},
		{`{"error": "boom"}`, false, ""},
		{`not json`, false, ""},
	}
	for _, tt := range tests {
		connected, session := parseStatus(tt.text)
		if connected != tt.wantConnected || session != tt.wantSession {
			t.Errorf("parseStatus(%q) = (%v, %q), want (%v, %q)", tt.text, connected, session, tt.wantConnected, tt.wantSession)
		}
		if isConnected(tt.text) != tt.wantConnected {
			t.Errorf("isConnected(%q) != %v", tt.text, tt.wantConnected)
		}
	}
}
