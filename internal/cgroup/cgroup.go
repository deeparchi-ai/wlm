// Package cgroup provides read/write access to cgroup v2 files.
// All operations use the unified hierarchy at /sys/fs/cgroup.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const basePath = "/sys/fs/cgroup"

// CPUStat holds per-cgroup CPU statistics.
type CPUStat struct {
	UsageUsec  uint64 // total CPU time in microseconds
	UserUsec   uint64
	SystemUsec uint64
}

// PSIStats holds pressure stall information for a cgroup.
type PSIStats struct {
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
}

// ReadCPUStat reads cpu.stat from a cgroup.
func ReadCPUStat(cgroupPath string) (*CPUStat, error) {
	data, err := os.ReadFile(filepath.Join(basePath, cgroupPath, "cpu.stat"))
	if err != nil {
		return nil, fmt.Errorf("reading cpu.stat for %s: %w", cgroupPath, err)
	}
	s := &CPUStat{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "usage_usec":
			s.UsageUsec = v
		case "user_usec":
			s.UserUsec = v
		case "system_usec":
			s.SystemUsec = v
		}
	}
	return s, nil
}

// ReadCPUWeight reads the current cpu.weight value.
func ReadCPUWeight(cgroupPath string) (uint32, error) {
	data, err := os.ReadFile(filepath.Join(basePath, cgroupPath, "cpu.weight"))
	if err != nil {
		return 0, fmt.Errorf("reading cpu.weight for %s: %w", cgroupPath, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing cpu.weight for %s: %w", cgroupPath, err)
	}
	return uint32(v), nil
}

// WriteCPUWeight sets cpu.weight for a cgroup.
func WriteCPUWeight(cgroupPath string, weight uint32) error {
	path := filepath.Join(basePath, cgroupPath, "cpu.weight")
	return os.WriteFile(path, []byte(strconv.FormatUint(uint64(weight), 10)), 0644)
}

// ReadPSI reads cpu.pressure for a cgroup, or system-wide if cgroupPath is empty.
func ReadPSI(cgroupPath string) (*PSIStats, error) {
	var psiPath string
	if cgroupPath == "" {
		psiPath = "/proc/pressure/cpu"
	} else {
		psiPath = filepath.Join(basePath, cgroupPath, "cpu.pressure")
	}
	data, err := os.ReadFile(psiPath)
	if err != nil {
		return nil, fmt.Errorf("reading PSI from %s: %w", psiPath, err)
	}
	return parsePSILine(string(data))
}

// parsePSILine parses a PSI line like:
//   some avg10=0.00 avg60=0.00 avg300=0.00 total=12345
func parsePSILine(line string) (*PSIStats, error) {
	s := &PSIStats{}
	// Find the "some" line
	for _, part := range strings.Split(line, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(part), "some") {
			continue
		}
		fields := strings.Fields(part)
		for _, f := range fields {
			if strings.HasPrefix(f, "avg10=") {
				s.SomeAvg10, _ = strconv.ParseFloat(f[6:], 64)
			}
			if strings.HasPrefix(f, "avg60=") {
				s.SomeAvg60, _ = strconv.ParseFloat(f[6:], 64)
			}
			if strings.HasPrefix(f, "avg300=") {
				s.SomeAvg300, _ = strconv.ParseFloat(f[7:], 64)
			}
		}
		return s, nil
	}
	return s, nil
}

// EnsurePath creates the cgroup directory if it doesn't exist.
// Returns nil if it already exists, or an error if creation fails.
func EnsurePath(cgroupPath string) error {
	fullPath := filepath.Join(basePath, cgroupPath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return os.MkdirAll(fullPath, 0755)
	}
	return nil
}
