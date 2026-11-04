//go:build !windows

package sysinfo

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// On Linux everything comes from /proc. Elsewhere (macOS) these fail and the
// UI shows the metric as unavailable; the recorder is meant for Windows.

func cpuTimes() (idle, total uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, errors.New("unexpected /proc/stat")
	}
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		if i >= 8 { // guest time is already counted in user time
			break
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return idle, total, nil
}

func memory() (total, avail uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var gotT, gotA bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fs[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total, gotT = n*1024, true
		case "MemAvailable":
			avail, gotA = n*1024, true
		}
	}
	if !gotT || !gotA {
		return 0, 0, errors.New("unexpected /proc/meminfo")
	}
	return total, avail, nil
}

func uptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("unexpected /proc/uptime")
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(s * float64(time.Second)), nil
}

func netBytes() (rx, tx uint64, err error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return rx, tx, nil
}
