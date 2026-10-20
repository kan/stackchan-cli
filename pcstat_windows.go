//go:build windows

package main

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetSystemTimes and GlobalMemoryStatusEx are not in x/sys/windows.
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

func filetime(f windows.Filetime) uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

// sampleCPU reads the CPU time spent so far. Kernel time includes idle time.
func sampleCPU() (cpuSample, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return cpuSample{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	return cpuSample{at: time.Now(), idle: filetime(idle), total: filetime(kernel) + filetime(user)}, nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// memoryLoad is the percentage of physical memory in use (dwMemoryLoad).
func memoryLoad() (float64, error) {
	m := memoryStatusEx{}
	m.length = uint32(unsafe.Sizeof(m))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return float64(m.memoryLoad), nil
}

// diskSpace returns the free and total bytes of a drive ("C:").
func diskSpace(drive string) (free, total uint64, err error) {
	path, err := windows.UTF16PtrFromString(drive + `\`)
	if err != nil {
		return 0, 0, err
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(path, &avail, &total, &free); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", drive, err)
	}
	return free, total, nil
}
