// Package sysinfo reports the PC's state: disk usage for the recordings
// volume, and CPU, memory, network and uptime for the "Sistema" settings.
//
// Everything is read from counters the OS keeps anyway (no extra process, no
// WMI, no PowerShell), so asking for it costs next to nothing and can never
// get in the way of a recording.
package sysinfo
