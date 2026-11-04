package sysinfo

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

func ft(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// cpuTimes returns idle and total CPU time, all cores, in 100 ns units.
// Kernel time includes idle time.
func cpuTimes() (idle, total uint64, err error) {
	var i, k, u windows.Filetime
	r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, e
	}
	return ft(i), ft(k) + ft(u), nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memory() (total, avail uint64, err error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0, e
	}
	return m.TotalPhys, m.AvailPhys, nil
}

func uptime() (time.Duration, error) { return windows.DurationSinceBoot(), nil }

// netBytes adds up the traffic of the network cards that are up. Only
// hardware interfaces count: Windows also lists filter and virtual
// interfaces stacked on the same card, which would count it more than once.
func netBytes() (rx, tx uint64, err error) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		return 0, 0, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	found := false
	for i := range rows {
		r := &rows[i]
		const hardwareInterface = 1 // InterfaceAndOperStatusFlags bit
		if r.InterfaceAndOperStatusFlags&hardwareInterface == 0 || r.OperStatus != windows.IfOperStatusUp || r.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		rx += r.InOctets
		tx += r.OutOctets
		found = true
	}
	if !found {
		return 0, 0, errors.New("no network interface up")
	}
	return rx, tx, nil
}
