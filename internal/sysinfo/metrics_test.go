package sysinfo

import (
	"runtime"
	"testing"
)

func TestRead(t *testing.T) {
	var s Sampler
	m := s.Read()
	if m.CPUs < 1 {
		t.Fatalf("cpus = %d", m.CPUs)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if m.CPUPercent == nil || *m.CPUPercent < 0 || *m.CPUPercent > 100 {
		t.Errorf("cpu = %v", m.CPUPercent)
	}
	if m.MemTotal == nil || m.MemAvailable == nil || *m.MemAvailable > *m.MemTotal || *m.MemTotal == 0 {
		t.Errorf("memory = %v / %v", m.MemAvailable, m.MemTotal)
	}
	if m.UptimeSeconds == nil || *m.UptimeSeconds <= 0 {
		t.Errorf("uptime = %v", m.UptimeSeconds)
	}
	if m.NetRxRate == nil || m.NetTxRate == nil {
		t.Errorf("network = %v / %v", m.NetRxRate, m.NetTxRate)
	}
	// A second read right away reuses the previous sample's baseline.
	if m2 := s.Read(); m2.CPUPercent == nil {
		t.Errorf("second read: cpu = nil")
	}
}
