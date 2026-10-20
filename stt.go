package main

// Resident speech-to-text. Loading the faster-whisper model takes about as
// long as transcribing a short utterance, so the daemon keeps one Python
// process with the model loaded and sends it one request per utterance.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sttWorkerScript runs in the gateway's Python, which has faster-whisper and
// the model, with the settings of the gateway's listen (whisperEnv). It
// answers each request line {"path": ..., "prompt": ...} with one line {"text": ...} or
// {"error": ...}, after a first {"ready": true, "device": ...} once the model
// is loaded. It exits at EOF.
//
// On the GPU the first transcription also initializes CUDA (seconds), so the
// worker runs one on silence before it says ready. If the GPU fails, it falls
// back to the CPU and says why in "fallback".
const sttWorkerScript = `import json, os, sys
import numpy as np
from faster_whisper import WhisperModel
from faster_whisper.audio import decode_audio
def skip_click(a):
    # The device's recordings start with a loud click about 0.2-0.4 s in
    # (peak RMS ~0.55 over 20 ms, 5-30 times what follows), then quiet.
    # Whisper turns it into words ("いいよ、いいよ" for 調子どう), so start
    # after it. It is a click only when it is loud, within the first 0.6 s,
    # over within 0.3 s (60 ms below 0.02) and at least 3 times louder than
    # anything after it; loud speech is none of these. Returns the audio and
    # the samples skipped.
    f = 320  # 20 ms at 16 kHz
    rms = np.array([np.sqrt(np.mean(a[i:i + f] ** 2)) for i in range(0, len(a) - f + 1, f)])
    for i in np.flatnonzero(rms[:30] >= 0.2):
        for j in range(i + 1, min(i + 16, len(rms) - 2)):
            if (rms[j:j + 3] < 0.02).all():
                if rms[i:j].max() >= 3 * rms[j:].max():
                    return a[j * f:], j * f
                break
    return a, 0
def load(device, compute_type):
    m = WhisperModel(os.environ["STACKCHAN_FASTER_WHISPER_MODEL"], device=device, compute_type=compute_type)
    list(m.transcribe(np.zeros(16000, dtype=np.float32), language="ja", beam_size=1)[0])
    return m
device = os.getenv("STACKCHAN_FASTER_WHISPER_DEVICE") or "cpu"
compute_type = os.getenv("STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE") or "int8"
ready = {"ready": True}
try:
    m = load(device, compute_type)
except Exception as e:
    if device == "cpu":
        raise
    ready["fallback"] = f"{device} failed ({type(e).__name__}: {e})"
    device, compute_type = "cpu", "int8"
    m = load(device, compute_type)
def reply(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()
reply({**ready, "device": device + "/" + compute_type})
for line in sys.stdin:
    try:
        req = json.loads(line)
        audio, skipped = skip_click(decode_audio(req["path"], sampling_rate=16000))
        segments, _ = m.transcribe(audio, language="ja", beam_size=1, vad_filter=False,
                                   initial_prompt=req.get("prompt") or None)
        reply({"text": "".join(s.text for s in segments).strip(), "skipped_ms": skipped // 16})
    except Exception as e:
        reply({"error": f"{type(e).__name__}: {e}"})
`

type sttReply struct {
	Ready    bool   `json:"ready"`
	Device   string `json:"device"`   // with ready: what the model runs on
	Fallback string `json:"fallback"` // with ready: why the GPU was not used
	Text     string `json:"text"`
	Skipped  int    `json:"skipped_ms"` // with text: the device's click cut from the start
	Error    string `json:"error"`
}

// sttWorker owns the resident process. Requests are handled one at a time;
// the process starts on first use (or warm) and again after it died or was
// stopped for not answering in time.
type sttWorker struct {
	command     func() (*exec.Cmd, error) // builds the process; set by newSTTWorker
	loadTimeout time.Duration             // for the model load (a first use downloads it)
	timeout     time.Duration             // for each request

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string   // stdout lines; closed once the process has exited
	exited chan struct{} // closed once the process has exited and been reaped
	stderr *syncBuffer
}

// sttLoadTimeout allows a first download of the model (about 500MB for small).
const sttLoadTimeout = 5 * time.Minute

// errNoReply is the worker not answering within its timeout.
var errNoReply = errors.New("no result")

func newSTTWorker() *sttWorker {
	return &sttWorker{command: sttCommand, loadTimeout: sttLoadTimeout, timeout: transcribeTimeout}
}

// sttCommand builds the worker process in the gateway's Python.
func sttCommand() (*exec.Cmd, error) {
	py := sttPython()
	if py == "" {
		return nil, errors.New("the gateway's Python was not found (set STACKCHAN_STT_PYTHON)")
	}
	cmd := exec.Command(py, "-I", "-X", "utf8", "-c", sttWorkerScript)
	cmd.Env = append(os.Environ(), whisperEnv(py)...)
	hideWindow(cmd)
	return cmd, nil
}

// cudaLibDirs returns the DLL directories of the NVIDIA pip packages
// (nvidia-cublas-cu12, nvidia-cudnn-cu12) in the Python environment of py,
// or nil unless both are there. faster-whisper needs them on the GPU, and
// CTranslate2 finds them only on PATH.
func cudaLibDirs(py string) []string {
	if py == "" {
		return nil
	}
	nv := filepath.Join(filepath.Dir(filepath.Dir(py)), "Lib", "site-packages", "nvidia")
	if !fileExists(filepath.Join(nv, "cublas", "bin", "cublas64_12.dll")) ||
		!fileExists(filepath.Join(nv, "cudnn", "bin", "cudnn64_9.dll")) {
		return nil
	}
	// cuda_nvrtc comes with cuBLAS; a missing directory on PATH is harmless.
	return []string{
		filepath.Join(nv, "cublas", "bin"),
		filepath.Join(nv, "cudnn", "bin"),
		filepath.Join(nv, "cuda_nvrtc", "bin"),
	}
}

// whisperEnv returns the faster-whisper settings for the gateway's listen
// and the voice-command worker alike: the model (unless set) and, when the
// CUDA libraries are installed in py's environment, their directories on
// PATH and (unless set) device cuda. On cuda the compute type defaults to
// float16 (int8 is the CPU default).
func whisperEnv(py string) []string {
	var env []string
	if os.Getenv("STACKCHAN_FASTER_WHISPER_MODEL") == "" {
		env = append(env, "STACKCHAN_FASTER_WHISPER_MODEL="+defaultWhisperModel)
	}
	device := os.Getenv("STACKCHAN_FASTER_WHISPER_DEVICE")
	if dirs := cudaLibDirs(py); dirs != nil {
		env = append(env, "PATH="+strings.Join(dirs, string(os.PathListSeparator))+string(os.PathListSeparator)+os.Getenv("PATH"))
		if device == "" {
			device = "cuda"
			env = append(env, "STACKCHAN_FASTER_WHISPER_DEVICE=cuda")
		}
	}
	if device == "cuda" && os.Getenv("STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE") == "" {
		env = append(env, "STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE=float16")
	}
	return env
}

// warm starts the worker so the first utterance does not wait for the model.
func (w *sttWorker) warm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureStarted(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: voice: %v\n", err)
	}
}

// transcribe turns Ogg/Opus audio into text. prompt ("" for none) is
// whisper's initial prompt: words to expect (see voiceRules.sttPrompt).
func (w *sttWorker) transcribe(audio []byte, prompt string) (string, error) {
	f, err := os.CreateTemp(stackchanDir(), "voice-*.ogg")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(audio)
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureStarted(); err != nil {
		return "", err
	}
	req, _ := json.Marshal(map[string]string{"path": f.Name(), "prompt": prompt})
	if _, err := w.stdin.Write(append(req, '\n')); err != nil {
		w.stop()
		return "", fmt.Errorf("transcribe: %v", err)
	}
	r, err := w.next(w.timeout)
	if err != nil {
		return "", err
	}
	if r.Error != "" {
		return "", fmt.Errorf("transcribe: %s", r.Error)
	}
	if r.Skipped > 0 {
		fmt.Fprintf(os.Stderr, "daemon: voice: skipped the device's click (first %d ms)\n", r.Skipped)
	}
	return r.Text, nil
}

// ensureStarted starts the process and waits for the model, if not running.
// Called with mu held.
func (w *sttWorker) ensureStarted() error {
	if w.cmd != nil {
		return nil
	}
	cmd, err := w.command()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	w.stderr = &syncBuffer{}
	cmd.Stderr = w.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("transcribe: start: %v", err)
	}
	w.cmd, w.stdin, w.lines, w.exited = cmd, stdin, make(chan string, 4), make(chan struct{})
	go func(lines chan<- string, exited chan<- struct{}) {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		// A read error (or a line over 4MB) ends the stream like an exit; the
		// caller reports the stderr tail. Wait first: it finishes copying
		// stderr, so the tail is complete when lines closes.
		_ = cmd.Wait()
		close(exited)
		close(lines)
	}(w.lines, w.exited)

	r, err := w.next(w.loadTimeout)
	if errors.Is(err, errNoReply) {
		return fmt.Errorf("%w (loading the model; a first use downloads it, so a slow line can take longer)", err)
	}
	if err != nil {
		return err
	}
	if !r.Ready {
		w.stop()
		return fmt.Errorf("transcribe: unexpected first reply %+v", r)
	}
	if r.Fallback != "" {
		fmt.Fprintf(os.Stderr, "daemon: voice: %s; using the cpu\n", r.Fallback)
	}
	fmt.Fprintf(os.Stderr, "daemon: voice: speech recognition model loaded (%s)\n", r.Device)
	return nil
}

// next reads the worker's next reply. On a timeout or exit it stops the
// worker, so the next call starts a fresh one. Called with mu held.
func (w *sttWorker) next(timeout time.Duration) (sttReply, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case line, ok := <-w.lines:
			if !ok {
				msg := lastLine(w.stderr.String())
				w.stop()
				return sttReply{}, fmt.Errorf("transcribe: the speech recognizer exited: %s", msg)
			}
			if r, ok := parseSTTReply(line); ok {
				return r, nil
			}
			// Stray output, e.g. a library's print: skip it.
		case <-t.C:
			w.stop()
			return sttReply{}, fmt.Errorf("transcribe: %w within %s", errNoReply, timeout)
		}
	}
}

// parseSTTReply reads a reply line. Only a JSON object with "ready", "text"
// or "error" is one; anything else would put replies out of step with
// requests.
func parseSTTReply(line string) (sttReply, bool) {
	var keys map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &keys) != nil {
		return sttReply{}, false
	}
	_, ready := keys["ready"]
	_, text := keys["text"]
	_, failed := keys["error"]
	if !ready && !text && !failed {
		return sttReply{}, false
	}
	var r sttReply
	return r, json.Unmarshal([]byte(line), &r) == nil
}

// stop ends the process; the reader goroutine reaps it. Called with mu held.
func (w *sttWorker) stop() {
	if w.cmd == nil {
		return
	}
	_ = w.stdin.Close()
	select {
	case <-w.exited:
		// Already reaped: its PID may belong to another process by now.
	default:
		// The tree: a uv venv's python.exe runs the interpreter as its
		// child, which holds the model and the stdout pipe.
		killTree(w.cmd.Process)
	}
	// Drain what is left so the reader goroutine can finish and reap.
	go func(lines <-chan string) {
		for range lines {
		}
	}(w.lines)
	w.cmd, w.stdin, w.lines, w.exited = nil, nil, nil, nil
}

// syncBuffer keeps the tail of the process's stderr (for error messages),
// safe for the stderr copier and a reader at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

const syncBufferMax = 64 << 10

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
	if s.b.Len() > syncBufferMax {
		tail := append([]byte(nil), s.b.Bytes()[s.b.Len()-syncBufferMax/2:]...)
		s.b.Reset()
		s.b.Write(tail)
	}
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
