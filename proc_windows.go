//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killChildrenOnExit puts this process into a Job object that kills every
// member when the last handle closes, i.e. when this process exits for any
// reason (Ctrl-C, crash, Task Scheduler stop). Children spawned afterwards
// (gateway + its Python, VOICEVOX engine) inherit the job, so they never
// outlive us and keep ports 8765/8766/50021 busy.
// Breakaway is allowed so openInViewer can launch a viewer that survives us.
func killChildrenOnExit() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	// The handle is intentionally never closed: it closes when we exit.
}

// killTree kills p and its descendants. Kill alone misses them, e.g. the real
// interpreter that a uv venv's python.exe launcher starts as its child.
func killTree(p *os.Process) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	hideWindow(cmd)
	if cmd.Run() != nil {
		_ = p.Kill()
	}
}

// hideWindow makes a console child run without a visible window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// startDetached starts cmd outside our kill-on-exit job when possible, so it
// survives this process. Falls back to a normal start if breakaway is denied
// (e.g. an outer job that forbids it).
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := cmd.Start(); err == nil {
		return nil
	}
	retry := exec.Command(cmd.Path, cmd.Args[1:]...)
	return retry.Start()
}
