package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// noEngine makes any attempt to start an engine fail instead of launching a
// VOICEVOX installed on the test machine, and keeps voicevox.log out of the
// real home.
func noEngine(t *testing.T) {
	t.Helper()
	fakeHome(t)
	t.Setenv("STACKCHAN_VOICEVOX_ENGINE", filepath.Join(t.TempDir(), "missing-run.exe"))
	t.Setenv("STACKCHAN_VOICEVOX_AUTOSTART", "")
}

func TestWarmVoicevoxSynthesizesWithDefaultSpeaker(t *testing.T) {
	t.Setenv("STACKCHAN_VOICEVOX_DEFAULT_SPEAKER", "")
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+" speaker="+r.URL.Query().Get("speaker"))
		if r.URL.Path == "/audio_query" {
			_, _ = w.Write([]byte(`{"accent_phrases":[]}`))
		}
	}))
	defer srv.Close()

	if err := warmVoicevox(srv.URL); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, ", ")
	want := "POST /audio_query speaker=" + defaultVoicevoxSpeaker + ", POST /synthesis speaker=" + defaultVoicevoxSpeaker
	if got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}

// A synthesis error (e.g. the GPU cannot run it) must fail the warm-up, so
// the daemon restarts the engine on the CPU.
func TestWarmVoicevoxReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/synthesis" {
			http.Error(w, `{"detail":"inference failed"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := warmVoicevox(srv.URL); err == nil || !strings.Contains(err.Error(), "inference failed") {
		t.Errorf("err = %v, want the synthesis error", err)
	}
}

func TestEnsureVoicevoxUsesRunningEngine(t *testing.T) {
	noEngine(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`"0.25.0"`))
	}))
	defer srv.Close()
	t.Setenv("STACKCHAN_VOICEVOX_URL", srv.URL)
	if err := ensureVoicevox(); err != nil {
		t.Errorf("ensureVoicevox() = %v, want nil (engine already up)", err)
	}
}

// ensureVoicevox starts an engine only for these hosts.
func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"192.168.1.10", false},
		{"voicevox.example", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestEnsureVoicevoxAutostartDisabled(t *testing.T) {
	noEngine(t)
	t.Setenv("STACKCHAN_VOICEVOX_AUTOSTART", "0")
	t.Setenv("STACKCHAN_VOICEVOX_URL", "http://"+closedLocalAddr(t))
	err := ensureVoicevox()
	if err == nil || !strings.Contains(err.Error(), "auto-start disabled") {
		t.Errorf("err = %v, want the auto-start-disabled error", err)
	}
}

func TestEnsureVoicevoxReportsMissingEngine(t *testing.T) {
	noEngine(t)
	t.Setenv("STACKCHAN_VOICEVOX_URL", "http://"+closedLocalAddr(t))
	err := ensureVoicevox()
	if err == nil || !strings.Contains(err.Error(), "start VOICEVOX engine") {
		t.Errorf("err = %v, want a start failure", err)
	}
}
