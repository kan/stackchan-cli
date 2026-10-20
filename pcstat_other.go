//go:build !windows

package main

import "errors"

// The PC monitor reads Windows APIs only; elsewhere it reports that it cannot.
var errPCStatsUnsupported = errors.New("PC stats are only read on Windows")

func sampleCPU() (cpuSample, error) { return cpuSample{}, errPCStatsUnsupported }

func memoryLoad() (float64, error) { return 0, errPCStatsUnsupported }

func diskSpace(drive string) (free, total uint64, err error) {
	return 0, 0, errPCStatsUnsupported
}
