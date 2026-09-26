package monitor

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	commonv1 "github.com/Yesveer/wxt-agent/proto/common/v1"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
	"go.uber.org/zap"
)

// Monitor collects system resource statistics
type Monitor struct {
	logger           *zap.Logger
	lastNetworkStats map[string]net.IOCountersStat
	lastCheckTime    time.Time
}

// New creates a new monitor
func New(logger *zap.Logger) *Monitor {
	return &Monitor{
		logger:           logger,
		lastNetworkStats: make(map[string]net.IOCountersStat),
		lastCheckTime:    time.Now(),
	}
}

// GetStats returns current system resource statistics
func (m *Monitor) GetStats() (*commonv1.ResourceStats, error) {
	stats := &commonv1.ResourceStats{}

	// CPU usage
	cpuPercent, err := m.getCPUPercent()
	if err != nil {
		m.logger.Warn("Failed to get CPU usage", zap.Error(err))
		cpuPercent = 0
	}
	stats.CpuPercent = cpuPercent

	// Memory usage
	memPercent, err := m.getMemoryPercent()
	if err != nil {
		m.logger.Warn("Failed to get memory usage", zap.Error(err))
		memPercent = 0
	}
	stats.MemoryPercent = memPercent

	// Disk usage
	diskPercent, err := m.getDiskPercent()
	if err != nil {
		m.logger.Warn("Failed to get disk usage", zap.Error(err))
		diskPercent = 0
	}
	stats.DiskPercent = diskPercent

	// Uptime
	uptime, err := m.getUptime()
	if err != nil {
		m.logger.Warn("Failed to get uptime", zap.Error(err))
		uptime = 0
	}
	stats.UptimeSeconds = uptime

	// Network stats
	networkIn, networkOut, err := m.getNetworkStats()
	if err != nil {
		m.logger.Warn("Failed to get network stats", zap.Error(err))
		networkIn = 0
		networkOut = 0
	}
	stats.NetworkInbound = networkIn
	stats.NetworkOutbound = networkOut

	return stats, nil
}

// GetMachineTotals returns the machine's hardware totals so absolute usage can be
// computed from the reported percentages: CPU cores, total RAM (MB), total disk (GB).
func (m *Monitor) GetMachineTotals() (cores int, memTotalMB int64, diskTotalGB int64) {
	cores = runtime.NumCPU()
	if c, err := cpu.Counts(true); err == nil && c > 0 {
		cores = c
	}
	// uint64 -> int64: would only overflow with exabyte-scale RAM/disk, far
	// beyond any real machine this agent runs on.
	if vm, err := mem.VirtualMemory(); err == nil {
		memTotalMB = int64(vm.Total / (1024 * 1024)) // #nosec G115
	}
	if d, err := disk.Usage("/"); err == nil {
		diskTotalGB = int64(d.Total / (1024 * 1024 * 1024)) // #nosec G115
	}
	return
}

// AgentStats is the agent PROCESS's own resource usage (not the whole machine).
type AgentStats struct {
	CPUPercent float64 `json:"cpu_percent"` // across all cores (may exceed 100)
	MemoryMB   float64 `json:"memory_mb"`   // resident set size
	Goroutines int     `json:"goroutines"`
	UptimeSec  int64   `json:"uptime_sec"`
}

// GetAgentStats returns the agent process's own CPU/memory/uptime.
func (m *Monitor) GetAgentStats() AgentStats {
	st := AgentStats{Goroutines: runtime.NumGoroutine()}
	p, err := process.NewProcess(int32(os.Getpid())) // #nosec G115 -- OS PIDs never approach int32's range
	if err != nil {
		return st
	}
	if pct, e := p.Percent(500 * time.Millisecond); e == nil {
		st.CPUPercent = pct
	}
	if mi, e := p.MemoryInfo(); e == nil && mi != nil {
		st.MemoryMB = float64(mi.RSS) / (1024 * 1024)
	}
	if ct, e := p.CreateTime(); e == nil && ct > 0 {
		st.UptimeSec = (time.Now().UnixMilli() - ct) / 1000
	}
	return st
}

// getCPUPercent returns CPU usage percentage
func (m *Monitor) getCPUPercent() (float32, error) {
	// Get CPU percent over 1 second interval
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil {
		return 0, err
	}

	if len(percentages) == 0 {
		return 0, fmt.Errorf("no CPU data available")
	}

	return float32(percentages[0]), nil
}

// getMemoryPercent returns memory usage percentage
func (m *Monitor) getMemoryPercent() (float32, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}

	return float32(vmStat.UsedPercent), nil
}

// getDiskPercent returns disk usage percentage
func (m *Monitor) getDiskPercent() (float32, error) {
	// Get root partition usage
	diskStat, err := disk.Usage("/")
	if err != nil {
		return 0, err
	}

	return float32(diskStat.UsedPercent), nil
}

// getUptime returns system uptime in seconds
func (m *Monitor) getUptime() (int64, error) {
	uptime, err := host.Uptime()
	if err != nil {
		return 0, err
	}

	return int64(uptime), nil // #nosec G115 -- uptime in seconds would need ~292 billion years to overflow int64
}

// getNetworkStats returns network inbound and outbound in MB/s
func (m *Monitor) getNetworkStats() (float32, float32, error) {
	// Get network IO counters
	netStats, err := net.IOCounters(true)
	if err != nil {
		return 0, 0, err
	}

	if len(netStats) == 0 {
		return 0, 0, fmt.Errorf("no network data available")
	}

	// Calculate time delta
	now := time.Now()
	timeDelta := now.Sub(m.lastCheckTime).Seconds()
	if timeDelta <= 0 {
		timeDelta = 1 // Avoid division by zero
	}

	var totalBytesRecv, totalBytesSent uint64

	// Sum up all interfaces
	for _, stat := range netStats {
		// Skip loopback interface
		if strings.Contains(strings.ToLower(stat.Name), "lo") {
			continue
		}

		lastStat, exists := m.lastNetworkStats[stat.Name]
		if exists {
			// Calculate delta
			bytesRecv := stat.BytesRecv - lastStat.BytesRecv
			bytesSent := stat.BytesSent - lastStat.BytesSent
			totalBytesRecv += bytesRecv
			totalBytesSent += bytesSent
		}

		// Update last stats
		m.lastNetworkStats[stat.Name] = stat
	}

	// Update last check time
	m.lastCheckTime = now

	// Convert to MB/s
	networkInbound := float32(float64(totalBytesRecv) / timeDelta / 1024 / 1024)
	networkOutbound := float32(float64(totalBytesSent) / timeDelta / 1024 / 1024)

	return networkInbound, networkOutbound, nil
}

// GetOSInfo returns operating system information
func GetOSInfo() string {
	info, err := host.Info()
	if err != nil {
		return runtime.GOOS
	}

	return fmt.Sprintf("%s %s %s", info.Platform, info.PlatformVersion, info.KernelArch)
}

// GetIPAddress returns the primary IP address
func GetIPAddress() (string, error) {
	netInterfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1", err
	}

	for _, iface := range netInterfaces {
		// Skip loopback interfaces
		if strings.Contains(strings.ToLower(iface.Name), "lo") {
			continue
		}

		// Get addresses for this interface
		addrs := iface.Addrs
		for _, addr := range addrs {
			ip := addr.Addr
			// Skip IPv6 and loopback
			if strings.Contains(ip, ":") || strings.HasPrefix(ip, "127.") {
				continue
			}

			// Extract IP without CIDR
			if idx := strings.Index(ip, "/"); idx > 0 {
				ip = ip[:idx]
			}

			return ip, nil
		}
	}

	// Try hostname resolution as fallback
	hostname, err := os.Hostname()
	if err == nil && hostname != "" {
		// Just return a default if we can't find IP
		return "127.0.0.1", nil
	}

	return "127.0.0.1", nil
}
