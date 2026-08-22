package worker

import "sync"

// sysStat is a node's own view of the machine it runs on: what the panel's
// server list shows next to each node. Everything here is best-effort — a
// reading the platform cannot supply stays zero and the panel renders it blank,
// which is what it did for every field before this existed.
type sysStat struct {
	CPUPercent    float64 `json:"cpu_percent"`
	RAMPercent    float64 `json:"ram_percent"`
	RAMUsedMB     int     `json:"ram_used_mb"`
	RAMTotalMB    int     `json:"ram_total_mb"`
	DiskUsedGB    float64 `json:"disk_used_gb"`
	DiskTotalGB   float64 `json:"disk_total_gb"`
	NetInMbps     float64 `json:"net_in_mbps"`
	NetOutMbps    float64 `json:"net_out_mbps"`
	LinkSpeedMbps int     `json:"link_speed_mbps"`
}

// CPU and network are rates, so they are the difference between this reading
// and the previous one. The panel polls health on a timer, which is exactly the
// sampling interval wanted; the first call after startup has nothing to
// subtract from and reports zero for those two.
var statMu sync.Mutex

// readSysStat samples the machine. Safe for concurrent use.
func readSysStat() sysStat {
	statMu.Lock()
	defer statMu.Unlock()
	return sampleSysStat()
}
