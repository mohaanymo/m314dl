package worker

import (
	"encoding/json"
	"runtime"
	"testing"
)

// The panel reads these fields straight off /api/health, so the check is that a
// real reading comes back plausible and lands at the top level of the JSON —
// an embedded struct that stopped flattening would blank the whole server list
// again without failing anything else.
func TestSysStatReadsAndFlattens(t *testing.T) {
	body, err := json.Marshal(healthOut{Status: "ok", Channels: 1, Max: 32,
		Version: "0.0.0-test", sysStat: readSysStat()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "channels", "max", "version",
		"cpu_percent", "ram_percent", "ram_used_mb", "ram_total_mb",
		"disk_used_gb", "disk_total_gb", "net_in_mbps", "net_out_mbps",
		"link_speed_mbps"} {
		if _, ok := m[k]; !ok {
			t.Errorf("health is missing %q: %s", k, body)
		}
	}
	if m["version"] == "" {
		t.Error(`version is empty; the panel reads that as "not deployed"`)
	}

	if runtime.GOOS != "linux" {
		return
	}
	s := readSysStat()
	if s.RAMTotalMB <= 0 || s.RAMUsedMB <= 0 || s.RAMUsedMB > s.RAMTotalMB {
		t.Errorf("ram: used %d of %d MB", s.RAMUsedMB, s.RAMTotalMB)
	}
	if s.DiskTotalGB <= 0 || s.DiskUsedGB > s.DiskTotalGB {
		t.Errorf("disk: used %.1f of %.1f GB", s.DiskUsedGB, s.DiskTotalGB)
	}
	// CPU and network are rates: the first reading has no earlier sample to
	// subtract from and must report 0 rather than a garbage spike.
	if s.CPUPercent < 0 || s.CPUPercent > 100 {
		t.Errorf("cpu %.1f%% out of range", s.CPUPercent)
	}
	// A second reading has an interval behind it and must stay in range.
	s2 := readSysStat()
	if s2.CPUPercent < 0 || s2.CPUPercent > 100 {
		t.Errorf("cpu %.1f%% out of range on second sample", s2.CPUPercent)
	}
	if s2.NetInMbps < 0 || s2.NetOutMbps < 0 {
		t.Errorf("negative throughput: in %.1f out %.1f", s2.NetInMbps, s2.NetOutMbps)
	}
}
