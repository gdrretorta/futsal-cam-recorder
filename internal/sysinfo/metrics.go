package sysinfo

import (
	"runtime"
	"sync"
	"time"
)

// started is when this process started, close enough.
var started = time.Now()

// Metrics is a snapshot of the PC. A field is nil when the OS couldn't
// report it.
type Metrics struct {
	CPUs          int      `json:"cpus"`
	CPUPercent    *float64 `json:"cpuPercent,omitempty"`
	MemTotal      *uint64  `json:"memTotal,omitempty"`
	MemAvailable  *uint64  `json:"memAvailable,omitempty"`
	NetRxRate     *float64 `json:"netRxRate,omitempty"` // bytes per second
	NetTxRate     *float64 `json:"netTxRate,omitempty"`
	UptimeSeconds *int64   `json:"uptimeSeconds,omitempty"`
	// ProcessSeconds is how long this program has been running (since the
	// last restart or update).
	ProcessSeconds int64 `json:"processSeconds"`
}

type sample struct {
	at          time.Time
	cpuOK       bool
	idle, total uint64
	netOK       bool
	rx, tx      uint64
}

func take() sample {
	s := sample{at: time.Now()}
	if idle, total, err := cpuTimes(); err == nil {
		s.idle, s.total, s.cpuOK = idle, total, true
	}
	if rx, tx, err := netBytes(); err == nil {
		s.rx, s.tx, s.netOK = rx, tx, true
	}
	return s
}

// Sampler computes rates (CPU use, network traffic) between two calls of
// Read. The zero value is ready to use.
type Sampler struct {
	mu   sync.Mutex
	last sample
}

// maxGap is how old the previous sample may be to compute a rate from; past
// it (e.g. the first call), Read takes two samples gap apart.
const (
	maxGap = 30 * time.Second
	gap    = 500 * time.Millisecond
)

// Read returns the current metrics.
func (s *Sampler) Read() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.last
	if prev.at.IsZero() || time.Since(prev.at) > maxGap || time.Since(prev.at) < gap/2 {
		prev = take()
		time.Sleep(gap)
	}
	cur := take()
	s.last = cur

	m := Metrics{CPUs: runtime.NumCPU(), ProcessSeconds: int64(time.Since(started).Seconds())}
	if prev.cpuOK && cur.cpuOK && cur.total > prev.total && cur.idle >= prev.idle {
		dt, di := float64(cur.total-prev.total), float64(cur.idle-prev.idle)
		p := min(max((dt-di)/dt*100, 0), 100)
		m.CPUPercent = &p
	}
	if secs := cur.at.Sub(prev.at).Seconds(); prev.netOK && cur.netOK && secs > 0 && cur.rx >= prev.rx && cur.tx >= prev.tx {
		rx, tx := float64(cur.rx-prev.rx)/secs, float64(cur.tx-prev.tx)/secs
		m.NetRxRate, m.NetTxRate = &rx, &tx
	}
	if total, avail, err := memory(); err == nil {
		m.MemTotal, m.MemAvailable = &total, &avail
	}
	if up, err := uptime(); err == nil {
		u := int64(up.Seconds())
		m.UptimeSeconds = &u
	}
	return m
}
