package controlplane

import (
	"bufio"
	"context"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runtime measurements are sampled at most once per second, independently of
// dashboard range aggregation. Missing OS counters remain nullable.
type runtimeProbe struct {
	mu                   sync.Mutex
	at                   time.Time
	cpu                  float64
	rx, tx               uint64
	value                Map
	kernel, distribution string
}

func runtimeCommand(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func numericFile(path string) (float64, bool) {
	b, e := os.ReadFile(path)
	if e != nil {
		return 0, false
	}
	n, e := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return n, e == nil
}
func linuxMemory() (total, available float64, known bool) {
	f, e := os.Open("/proc/meminfo")
	if e != nil {
		return
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			available = n * 1024
			known = true
		}
	}
	return
}
func linuxNetwork() (rx, tx uint64) {
	b, e := os.ReadFile("/proc/net/dev")
	if e != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, data, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(data)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return
}
func (s *Server) runtimeSnapshot() Map {
	p := &s.runtimeStats
	p.mu.Lock()
	defer p.mu.Unlock()
	wall := time.Now()
	if !p.at.IsZero() && wall.Sub(p.at) < time.Second {
		return copyMap(p.value)
	}
	host, _ := os.Hostname()
	name := orValue(os.Getenv("CHATGPT2API_INSTANCE_NAME"), host)
	mode := "native"
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if info, e := os.Stat(marker); e == nil && info.Mode().IsRegular() {
			mode = "docker"
			break
		}
	}
	if p.kernel == "" {
		p.kernel = runtimeCommand("uname", "-r")
		if p.kernel == "" {
			p.kernel = "unknown"
		}
	}
	if p.distribution == "" {
		p.distribution = runtime.GOOS
		if runtime.GOOS == "darwin" {
			if v := runtimeCommand("sw_vers", "-productVersion"); v != "" {
				p.distribution = "macOS " + v
			}
		} else if b, e := os.ReadFile("/etc/os-release"); e == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
					p.distribution = strings.Trim(v, "\"'")
					break
				}
			}
		}
	}
	capacity := runtimeCPUCapacity(mode == "docker")
	scope := "system"
	if mode == "docker" {
		scope = "visible"
	}
	var cpuPercent, memoryBytes, processMemoryPercent, memoryPercent, storagePercent, rxRate, txRate any
	var usage unix.Rusage
	cpu := 0.0
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) == nil {
		cpu = float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	total, available := 0.0, 0.0
	memoryKnown := false
	rss := 0.0
	if runtime.GOOS == "linux" {
		total, available, memoryKnown = linuxMemory()
		if b, e := os.ReadFile("/proc/self/statm"); e == nil {
			f := strings.Fields(string(b))
			if len(f) > 1 {
				n, _ := strconv.ParseFloat(f[1], 64)
				rss = n * float64(os.Getpagesize())
			}
		}
		if n, ok := numericFile("/sys/fs/cgroup/memory.max"); ok && mode == "docker" && n > 0 && (total == 0 || n <= total) {
			if current, ok := numericFile("/sys/fs/cgroup/memory.current"); ok {
				total, available, scope = n, max(0, n-current), "container"
				memoryKnown = true
			}
		} else if n, ok := numericFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok && mode == "docker" && n > 0 && n <= total {
			if current, ok := numericFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok {
				total, available, scope = n, max(0, n-current), "container"
				memoryKnown = true
			}
		}
		rx, tx := linuxNetwork()
		if !p.at.IsZero() {
			seconds := wall.Sub(p.at).Seconds()
			if rx >= p.rx {
				rxRate = float64(rx-p.rx) / seconds
			}
			if tx >= p.tx {
				txRate = float64(tx-p.tx) / seconds
			}
		}
		p.rx, p.tx = rx, tx
	} else if runtime.GOOS == "darwin" {
		if n, e := strconv.ParseFloat(runtimeCommand("sysctl", "-n", "hw.memsize"), 64); e == nil {
			total = n
		}
		if v := strings.Fields(runtimeCommand("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid()))); len(v) > 0 {
			n, _ := strconv.ParseFloat(v[0], 64)
			rss = n * 1024
		}
		raw := runtimeCommand("vm_stat")
		pageSize := float64(os.Getpagesize())
		free := 0.0
		for _, line := range strings.Split(raw, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if k == "Pages free" || k == "Pages inactive" || k == "Pages speculative" {
				n, _ := strconv.ParseFloat(strings.Trim(strings.TrimSpace(v), "."), 64)
				free += n
				memoryKnown = true
			}
		}
		available = free * pageSize
	}
	if rss > 0 {
		memoryBytes = int64(rss)
		if total > 0 {
			processMemoryPercent = min(100, rss*100/total)
		}
	}
	if memoryKnown && total > 0 && available >= 0 {
		memoryPercent = min(100, max(0, (total-available)*100/total))
	}
	if !p.at.IsZero() {
		seconds := wall.Sub(p.at).Seconds()
		cpuPercent = min(100, max(0, (cpu-p.cpu)*100/seconds/capacity))
	}
	var fs unix.Statfs_t
	if unix.Statfs(filepath.Clean(s.DataDir), &fs) == nil && fs.Blocks > 0 {
		storagePercent = min(100, max(0, (float64(fs.Blocks)-float64(fs.Bfree))*100/float64(fs.Blocks)))
	}
	p.value = Map{"runtime_mode": mode, "instance_name": name, "distribution": p.distribution, "kernel_version": p.kernel, "architecture": runtime.GOARCH, "python_version": runtime.Version(), "cpu_capacity": capacity, "service_started_at": s.started.In(imageTaskZone).Format(time.RFC3339), "service_uptime_seconds": max(0, int(s.now().Sub(s.started).Seconds())), "process_cpu_percent": cpuPercent, "process_memory_bytes": memoryBytes, "process_memory_percent": processMemoryPercent, "memory_scope": scope, "memory_percent": memoryPercent, "storage_percent": storagePercent, "network_rx_bytes_per_sec": rxRate, "network_tx_bytes_per_sec": txRate}
	p.at, p.cpu = wall, cpu
	return copyMap(p.value)
}
