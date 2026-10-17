package main

// Auto-start of the VOICEVOX engine (vv-engine/run.exe) as a hidden child, so
// `say` works without opening the VOICEVOX GUI. If an engine is already
// answering on the configured URL (GUI or a user-started engine), it is used
// as-is. An engine we start lives in our kill-on-exit job (killChildrenOnExit),
// so it stops when this process exits.
//
// Environment:
//   STACKCHAN_VOICEVOX_URL        engine URL (default http://127.0.0.1:50021)
//   STACKCHAN_VOICEVOX_ENGINE     path to run.exe (default: standard install dirs)
//   STACKCHAN_VOICEVOX_AUTOSTART  "0" disables auto-start

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const voicevoxReadyTimeout = 60 * time.Second

// voicevoxMu serializes ensureVoicevox so concurrent callers (daemon startup
// and a say) don't start two engines.
var voicevoxMu sync.Mutex

var voicevoxHTTP = &http.Client{Timeout: 500 * time.Millisecond}

func voicevoxURL() string {
	if u := os.Getenv("STACKCHAN_VOICEVOX_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:50021"
}

func voicevoxUp(base string) bool {
	resp, err := voicevoxHTTP.Get(base + "/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func voicevoxEnginePath() string {
	if p := os.Getenv("STACKCHAN_VOICEVOX_ENGINE"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "VOICEVOX", "vv-engine", "run.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "VOICEVOX", "vv-engine", "run.exe"),
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// ensureVoicevox makes sure an engine answers on voicevoxURL(), starting one if
// needed. It blocks until the engine is ready (a cold start takes a few
// seconds). Errors are returned for the caller to report; say still runs and
// the gateway reports its own TTS error.
func ensureVoicevox() error {
	voicevoxMu.Lock()
	defer voicevoxMu.Unlock()

	base := voicevoxURL()
	if voicevoxUp(base) {
		return nil
	}
	if os.Getenv("STACKCHAN_VOICEVOX_AUTOSTART") == "0" {
		return fmt.Errorf("VOICEVOX not reachable at %s (auto-start disabled)", base)
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("STACKCHAN_VOICEVOX_URL: %w", err)
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("VOICEVOX not reachable at %s (remote host; not auto-starting)", base)
	}
	port := u.Port()
	if port == "" {
		port = "50021"
	}
	exe := voicevoxEnginePath()
	if exe == "" {
		return fmt.Errorf("VOICEVOX not reachable at %s and engine run.exe not found (set STACKCHAN_VOICEVOX_ENGINE)", base)
	}

	cmd := exec.Command(exe, "--host", "127.0.0.1", "--port", port)
	hideWindow(cmd)
	if logf, err := openStackchanLog("voicevox.log"); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close() // the child holds its own handle
	}
	fmt.Fprintf(os.Stderr, "voicevox: starting engine %s (port %s)...\n", exe, port)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VOICEVOX engine: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(voicevoxReadyTimeout)
	for !voicevoxUp(base) {
		select {
		case <-exited:
			return fmt.Errorf("VOICEVOX engine exited during startup (see ~/.stackchan/voicevox.log)")
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return fmt.Errorf("VOICEVOX engine not ready within %s", voicevoxReadyTimeout)
		}
	}
	fmt.Fprintln(os.Stderr, "voicevox: engine ready")
	return nil
}

// openStackchanLog opens ~/.stackchan/<name> for appending.
func openStackchanLog(name string) (*os.File, error) {
	dir := stackchanDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
