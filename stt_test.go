package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSTTHelperProcess is not a test: it is the fake worker the tests below
// start (the test binary re-run with STT_HELPER set). Like the real one it
// says ready, then answers each request; the audio file's content picks the
// answer.
func TestSTTHelperProcess(t *testing.T) {
	mode := os.Getenv("STT_HELPER")
	if mode == "" {
		return
	}
	if mode == "crash-at-start" {
		fmt.Fprintln(os.Stderr, "RuntimeError: CUDA failed")
		os.Exit(1)
	}
	fmt.Println("loading model...") // stray output is skipped
	fmt.Println(`{"progress": 1}`)  // and so is JSON that is not a reply
	fmt.Println(`{"ready": true}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct{ Path string }
		_ = json.Unmarshal(sc.Bytes(), &req)
		audio, _ := os.ReadFile(req.Path)
		switch string(audio) {
		case "hang":
			time.Sleep(time.Minute)
		case "crash":
			fmt.Fprintln(os.Stderr, "Traceback ...\nValueError: bad audio")
			os.Exit(1)
		case "fail":
			fmt.Println(`{"error": "ValueError: bad audio"}`)
		case "silence":
			fmt.Println(`{}`)
			fmt.Println(`{"text": ""}`)
		default:
			fmt.Printf(`{"text": "heard %s (pid %d)"}`+"\n", audio, os.Getpid())
		}
	}
	os.Exit(0)
}

func helperWorker(t *testing.T, mode string) *sttWorker {
	t.Helper()
	fakeHome(t)
	if err := os.MkdirAll(stackchanDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &sttWorker{
		loadTimeout: 5 * time.Second,
		timeout:     2 * time.Second,
		command: func() (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSTTHelperProcess$")
			cmd.Env = append(os.Environ(), "STT_HELPER="+mode)
			return cmd, nil
		},
	}
	t.Cleanup(func() {
		w.mu.Lock()
		w.stop()
		w.mu.Unlock()
	})
	return w
}

func pidOf(text string) string { return text[strings.LastIndex(text, "pid"):] }

func TestSTTWorkerKeepsOneProcess(t *testing.T) {
	w := helperWorker(t, "ok")
	first, err := w.transcribe([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "heard hello") {
		t.Errorf("text = %q", first)
	}
	second, err := w.transcribe([]byte("again"))
	if err != nil {
		t.Fatal(err)
	}
	if pidOf(first) != pidOf(second) {
		t.Errorf("a second process answered (%s, then %s); want the model kept loaded", pidOf(first), pidOf(second))
	}
}

func TestSTTWorkerErrorKeepsProcess(t *testing.T) {
	w := helperWorker(t, "ok")
	first, _ := w.transcribe([]byte("a"))
	if _, err := w.transcribe([]byte("fail")); err == nil || !strings.Contains(err.Error(), "bad audio") {
		t.Errorf("err = %v, want the worker's error", err)
	}
	after, err := w.transcribe([]byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if pidOf(first) != pidOf(after) {
		t.Error("an error reply restarted the worker")
	}
}

// An empty text is a reply, a JSON line without reply keys is not: replies
// stay in step with requests.
func TestSTTWorkerKeepsRepliesInStep(t *testing.T) {
	w := helperWorker(t, "ok")
	if text, err := w.transcribe([]byte("silence")); err != nil || text != "" {
		t.Errorf("silence: text %q, err %v; want empty text", text, err)
	}
	text, err := w.transcribe([]byte("next"))
	if err != nil || !strings.HasPrefix(text, "heard next") {
		t.Errorf("after silence: text %q, err %v; want the reply to this request", text, err)
	}
}

func TestSTTWorkerRestartsAfterCrash(t *testing.T) {
	w := helperWorker(t, "ok")
	first, _ := w.transcribe([]byte("a"))
	if _, err := w.transcribe([]byte("crash")); err == nil || !strings.Contains(err.Error(), "bad audio") {
		t.Errorf("err = %v, want the stderr tail", err)
	}
	after, err := w.transcribe([]byte("b"))
	if err != nil {
		t.Fatalf("after a crash: %v", err)
	}
	if pidOf(first) == pidOf(after) {
		t.Error("the crashed worker answered again")
	}
}

func TestSTTWorkerTimeout(t *testing.T) {
	w := helperWorker(t, "ok")
	w.timeout = 500 * time.Millisecond
	if _, err := w.transcribe([]byte("hang")); err == nil || !strings.Contains(err.Error(), "no result within") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if _, err := w.transcribe([]byte("b")); err != nil {
		t.Errorf("after a timeout: %v", err)
	}
}

func TestSTTWorkerStartFailure(t *testing.T) {
	w := helperWorker(t, "crash-at-start")
	_, err := w.transcribe([]byte("a"))
	if err == nil || !strings.Contains(err.Error(), "CUDA failed") {
		t.Errorf("err = %v, want the start error from stderr", err)
	}
	if strings.Contains(err.Error(), "download") {
		t.Errorf("err = %v: the download hint is only for a load that times out", err)
	}
}

func TestWhisperEnv(t *testing.T) {
	root := t.TempDir()
	py := filepath.Join(root, "Scripts", "python.exe")
	t.Setenv("STACKCHAN_FASTER_WHISPER_MODEL", "")
	t.Setenv("STACKCHAN_FASTER_WHISPER_DEVICE", "")
	t.Setenv("STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE", "")
	usesGPU := func() bool { return strings.Contains(strings.Join(whisperEnv(py), "\n"), "DEVICE=cuda") }

	env := strings.Join(whisperEnv(py), "\n")
	if env != "STACKCHAN_FASTER_WHISPER_MODEL="+defaultWhisperModel {
		t.Errorf("no CUDA libraries: env = %q, want only the default model (cpu)", env)
	}

	nv := filepath.Join(root, "Lib", "site-packages", "nvidia")
	writeDLL := func(lib, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(nv, lib, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nv, lib, "bin", name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeDLL("cublas", "cublas64_12.dll")
	if usesGPU() {
		t.Error("cuBLAS without cuDNN: chose the GPU (the gateway's listen would fail to load)")
	}
	writeDLL("cudnn", "cudnn64_9.dll")
	env = strings.Join(whisperEnv(py), "\n")
	for _, want := range []string{
		filepath.Join(nv, "cublas", "bin"),
		filepath.Join(nv, "cudnn", "bin"),
		"STACKCHAN_FASTER_WHISPER_DEVICE=cuda",
		"STACKCHAN_FASTER_WHISPER_COMPUTE_TYPE=float16",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}

	// cuda chosen by hand gets float16 too.
	t.Setenv("STACKCHAN_FASTER_WHISPER_DEVICE", "cuda")
	if env := strings.Join(whisperEnv(py), "\n"); !strings.Contains(env, "COMPUTE_TYPE=float16") {
		t.Errorf("DEVICE=cuda by hand: env lacks float16:\n%s", env)
	}

	// Settings the user chose are kept; only PATH is added.
	t.Setenv("STACKCHAN_FASTER_WHISPER_MODEL", "medium")
	t.Setenv("STACKCHAN_FASTER_WHISPER_DEVICE", "cpu")
	env = strings.Join(whisperEnv(py), "\n")
	if strings.Contains(env, "MODEL=") || strings.Contains(env, "DEVICE=") || strings.Contains(env, "COMPUTE_TYPE=") {
		t.Errorf("overrode the user's settings:\n%s", env)
	}
}
