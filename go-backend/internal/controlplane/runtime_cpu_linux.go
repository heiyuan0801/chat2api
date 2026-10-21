//go:build linux

package controlplane

import (
	"golang.org/x/sys/unix"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func runtimeCPUCapacity(container bool) float64 {
	capacity := float64(runtime.NumCPU())
	var affinity unix.CPUSet
	if unix.SchedGetaffinity(0, &affinity) == nil && affinity.Count() > 0 {
		capacity = min(capacity, float64(affinity.Count()))
	}
	if !container {
		return capacity
	}
	if b, e := os.ReadFile("/sys/fs/cgroup/cpu.max"); e == nil {
		f := strings.Fields(string(b))
		if len(f) >= 2 {
			q, qe := strconv.ParseFloat(f[0], 64)
			p, pe := strconv.ParseFloat(f[1], 64)
			if qe == nil && pe == nil && q > 0 && p > 0 {
				return min(capacity, q/p)
			}
		}
	}
	for _, base := range []string{"/sys/fs/cgroup/cpu/", "/sys/fs/cgroup/"} {
		quota, qok := numericFile(base + "cpu.cfs_quota_us")
		period, pok := numericFile(base + "cpu.cfs_period_us")
		if qok && pok && quota > 0 && period > 0 {
			return min(capacity, quota/period)
		}
	}
	return capacity
}
