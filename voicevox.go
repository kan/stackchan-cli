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
//   STACKCHAN_VOICEVOX_GPU        "0" starts the engine on the CPU (default: GPU,
//                                 falling back to the CPU if it fails to start)

import (
	"bytes"
	"fmt"
	"io"
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

// defaultVoicevoxSpeaker is the personal default voice: 春日部つむぎ.
const defaultVoicevoxSpeaker = "8"

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
	if !isLoopbackHost(u.Hostname()) {
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

	// The GPU cuts synthesis about tenfold (1.1s → 0.1s for a sentence), and
	// a sentence is synthesized whole before the device hears any of it. Its
	// first synthesis takes about 5 s, though, so only an engine the daemon
	// keeps runs on the GPU (warmed up once); a one-shot say uses the CPU.
	if voicevoxResident && os.Getenv("STACKCHAN_VOICEVOX_GPU") != "0" {
		cmd, err := startVoicevox(exe, port, base, true)
		if err == nil {
			if err = warmVoicevox(base); err == nil {
				return nil
			}
			killTree(cmd.Process) // up, but cannot synthesize on the GPU
		}
		fmt.Fprintf(os.Stderr, "voicevox: gpu: %v; retrying on the cpu\n", err)
	}
	_, err = startVoicevox(exe, port, base, false)
	return err
}

// voicevoxResident is set by the daemon, which keeps the engine it starts.
var voicevoxResident bool

// startVoicevox starts the engine and waits until it answers.
func startVoicevox(exe, port, base string, gpu bool) (*exec.Cmd, error) {
	args := []string{"--host", "127.0.0.1", "--port", port}
	mode := "cpu"
	if gpu {
		args, mode = append(args, "--use_gpu"), "gpu"
	}
	cmd := exec.Command(exe, args...)
	hideWindow(cmd)
	if logf, err := openStackchanLog("voicevox.log"); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close() // the child holds its own handle
	}
	fmt.Fprintf(os.Stderr, "voicevox: starting engine %s (port %s, %s)...\n", exe, port, mode)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start VOICEVOX engine: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(voicevoxReadyTimeout)
	for !voicevoxUp(base) {
		select {
		case <-exited:
			return nil, fmt.Errorf("VOICEVOX engine exited during startup (see ~/.stackchan/voicevox.log)")
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			killTree(cmd.Process)
			return nil, fmt.Errorf("VOICEVOX engine not ready within %s", voicevoxReadyTimeout)
		}
	}
	fmt.Fprintf(os.Stderr, "voicevox: engine ready (%s)\n", mode)
	return cmd, nil
}

// warmVoicevox synthesizes a word with the default speaker once: on the GPU
// the first synthesis prepares it (about 5 s), which would otherwise delay
// the first say. An error means the engine cannot synthesize.
func warmVoicevox(base string) error {
	speaker := url.QueryEscape(os.Getenv("STACKCHAN_VOICEVOX_DEFAULT_SPEAKER"))
	if speaker == "" {
		speaker = defaultVoicevoxSpeaker
	}
	client := &http.Client{Timeout: 30 * time.Second}
	post := func(path, contentType string, body io.Reader) ([]byte, error) {
		resp, err := client.Post(base+path, contentType, body)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(data))
		}
		return data, err
	}
	query, err := post("/audio_query?speaker="+speaker+"&text="+url.QueryEscape("はい"), "", nil)
	if err == nil {
		_, err = post("/synthesis?speaker="+speaker, "application/json", bytes.NewReader(query))
	}
	if err != nil {
		return fmt.Errorf("warm-up: %w", err)
	}
	fmt.Fprintln(os.Stderr, "voicevox: warmed up")
	return nil
}

// isLoopbackHost reports whether host (a URL's hostname) is this machine,
// the only place an engine can be started.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openStackchanLog opens ~/.stackchan/<name> for appending.
func openStackchanLog(name string) (*os.File, error) {
	dir := stackchanDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
