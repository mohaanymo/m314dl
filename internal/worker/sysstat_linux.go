//go:build linux

package worker

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Previous samples, for the two readings that are rates rather than levels.
var (
	lastCPUBusy, lastCPUTotal float64
	lastNetIn, lastNetOut     float64
	lastNetAt                 time.Time
)

func sampleSysStat() sysStat {
	var s sysStat
	cpuPercent(&s)
	memStat(&s)
	diskStat(&s)
	netStat(&s)
	return s
}

// cpuPercent reads the aggregate jiffy counters and reports the share of the
// interval since the last reading that was not idle. iowait counts as idle:
// this box spent a day at 66% iowait, and reporting that as busy CPU would have
// pointed at the wrong bottleneck.
func cpuPercent(s *sysStat) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 6 || f[0] != "cpu" {
		return
	}
	var total, idle float64
	for i, v := range f[1:] {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	busy := total - idle
	if dt := total - lastCPUTotal; lastCPUTotal > 0 && dt > 0 {
		s.CPUPercent = round1((busy - lastCPUBusy) / dt * 100)
	}
	lastCPUBusy, lastCPUTotal = busy, total
}

// memStat reports used as total minus available. MemAvailable, not MemFree:
// page cache is reclaimable, and counting it as used reads as a machine at 95%
// memory that is in fact idle.
func memStat(s *sysStat) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	var totalKB, availKB float64
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB = n
		case "MemAvailable":
			availKB = n
		}
	}
	if totalKB <= 0 {
		return
	}
	s.RAMTotalMB = int(totalKB / 1024)
	s.RAMUsedMB = int((totalKB - availKB) / 1024)
	s.RAMPercent = round1((totalKB - availKB) / totalKB * 100)
}

// diskStat measures the root filesystem — the one that fills up and takes the
// worker with it. Used is total minus free-to-root, so the reserved blocks show
// as used, matching what `df` reports.
func diskStat(s *sysStat) {
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) != nil || st.Blocks == 0 {
		return
	}
	const gb = 1 << 30
	bs := float64(st.Bsize)
	s.DiskTotalGB = round1(float64(st.Blocks) * bs / gb)
	s.DiskUsedGB = round1(float64(st.Blocks-st.Bfree) * bs / gb)
}

// netStat sums every real interface: a node may relay over more than one, and
// the loopback traffic between a worker and a panel on the same host would
// otherwise double-count every byte the node serves.
func netStat(s *sysStat) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return
	}
	var in, out float64
	for _, line := range strings.Split(string(b), "\n")[2:] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseFloat(f[0], 64)
		tx, err2 := strconv.ParseFloat(f[8], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		in += rx
		out += tx
		if sp := linkSpeed(name); sp > s.LinkSpeedMbps {
			s.LinkSpeedMbps = sp
		}
	}
	now := time.Now()
	if secs := now.Sub(lastNetAt).Seconds(); !lastNetAt.IsZero() && secs > 0 {
		// Counters are cumulative bytes and wrap; a negative delta means the
		// counter reset (or the interface went away), so report nothing rather
		// than a nonsense spike.
		if d := in - lastNetIn; d >= 0 {
			s.NetInMbps = round1(d * 8 / secs / 1e6)
		}
		if d := out - lastNetOut; d >= 0 {
			s.NetOutMbps = round1(d * 8 / secs / 1e6)
		}
	}
	lastNetIn, lastNetOut, lastNetAt = in, out, now
}

// linkSpeed is the interface's negotiated speed in Mbit/s. Virtual interfaces
// report -1 or nothing at all; both come back as 0.
func linkSpeed(iface string) int {
	b, err := os.ReadFile("/sys/class/net/" + iface + "/speed")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
