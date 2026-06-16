package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"github.com/neoliv/arena/internal/stats"
	"github.com/neoliv/arena/internal/version"
)

var arenaStart = time.Now()

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<h1>Health</h1>`)

	// ── Arena Service ──
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	memPct := 0
	if m.Sys > 0 {
		var totalMem uint64
		if d, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(d), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					var kb uint64; fmt.Sscanf(strings.Fields(line)[1], "%d", &kb)
					totalMem = kb * 1024
					break
				}
			}
		}
		if totalMem > 0 { memPct = int(float64(m.Sys) / float64(totalMem) * 100) }
	}
	arenaDur := niceDuration(arenaStart)
	startStr := arenaStart.Format("2006-01-02 15:04")
	io.WriteString(w, `<h2>Arena Service</h2><table class="stats-table">`)
	kv := func(k, v, cls string) { fmt.Fprintf(w, `<tr class="%s"><td>%s</td><td>%s</td></tr>`, cls, k, v) }
	kv("Version", version.Version, "")
	kv("Online", arenaDur+" ("+startStr+")", "")
	kv("Goroutines", fmt.Sprintf("%d / %d cores", runtime.NumGoroutine(), runtime.NumCPU()), "")
	reqRate := stats.Global.ReqPerSec()
	inRate, outRate := stats.Global.ByteRate()
	kv("Requests", fmt.Sprintf("%.1f req/s (last minute)", reqRate), "")
	kv("Bandwidth", fmt.Sprintf("↓ %s/s  ↑ %s/s (last minute)", niceSize(int64(inRate)), niceSize(int64(outRate))), "")
	kv("Memory", fmt.Sprintf("%d%% (%s / %s)", memPct, niceSize(int64(m.Sys)), niceSize(int64(totalMem()))), memClass(memPct))

	// Disk stats
	dbPath := os.Getenv("ARENA_DB"); if dbPath == "" { dbPath = "/opt/arena/arena.db" }
	dbSize := fileSize(dbPath)
	backupDir := filepath.Join(filepath.Dir(dbPath), "backup")
	backupSize, backupCount := dirSize(backupDir)
	lastBackup := lastBackupFile(backupDir)
	totalPartition := totalDisk(filepath.Dir(dbPath))
	totalArena := dbSize + backupSize
	arenaDiskPct := 0
	if totalPartition > 0 { arenaDiskPct = int(float64(totalArena) / float64(totalPartition) * 100) }
	arenaDiskClass := ""; if arenaDiskPct > 90 { arenaDiskClass = "critical" } else if arenaDiskPct > 80 { arenaDiskClass = "warning" }
	kv("Disk", fmt.Sprintf("%d%% (%s / %s)", arenaDiskPct, niceSize(totalArena), niceSize(int64(totalPartition))), arenaDiskClass)
	kv("  database", fmt.Sprintf("%s (%s)", niceSize(dbSize), dbPath), "")
	kv("  backups", fmt.Sprintf("%s (%d file", niceSize(backupSize), backupCount)+func() string { if backupCount != 1 { return "s)" } else { return ")" } }(), "")
	kv("Last backup", lastBackup, "")
	io.WriteString(w, "</table>"+`</div>`+pageFoot)

	// ── System ──
	sysDur, sysStart := sysUptime()
	sysCPU := cpuPercent()
	cpuClass := ""; if sysCPU > 80 { cpuClass = "critical" } else if sysCPU > 50 { cpuClass = "warning" }
	sysMemPct, memUsed, memTotal := sysMemInfo()
	memClass2 := ""; if sysMemPct > 90 { memClass2 = "critical" } else if sysMemPct > 75 { memClass2 = "warning" }
	diskPct, diskUsed, diskTotal := sysDiskInfo()
	diskClass := ""; if diskPct > 90 { diskClass = "critical" } else if diskPct > 80 { diskClass = "warning" }

	io.WriteString(w, `<h2>System</h2><table class="stats-table">`)
	osName := readProcFile("/etc/os-release", "PRETTY_NAME")
	kernel := readProcFile("/proc/version", "")
	if i := strings.Index(kernel, " ("); i >= 0 { kernel = "Linux kernel " + kernel[:i] }
	kv("OS", osName+" ("+kernel+")", "")
	kv("Online", sysDur+" ("+sysStart+")", "")
	kv("CPU", fmt.Sprintf("%d%% (%d cores)", sysCPU, runtime.NumCPU()), cpuClass)
	kv("Memory", fmt.Sprintf("%d%% (%s / %s)", sysMemPct, niceSize(memUsed), niceSize(memTotal)), memClass2)
	kv("Disk", fmt.Sprintf("%d%% (%s / %s)", diskPct, niceSize(diskUsed), niceSize(diskTotal)), diskClass)
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

func fileSize(path string) int64 {
	s, err := os.Stat(path)
	if err != nil { return 0 }
	return s.Size()
}

func dirSize(path string) (int64, int) {
	var total int64
	var count int
	entries, err := os.ReadDir(path)
	if err != nil { return 0, 0 }
	for _, e := range entries {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil { total += info.Size(); count++ }
		}
	}
	return total, count
}

func lastBackupFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 { return "none" }
	var latest os.FileInfo
	for _, e := range entries {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				if latest == nil || info.ModTime().After(latest.ModTime()) { latest = info }
			}
		}
	}
	if latest == nil { return "none" }
	t := latest.ModTime()
	d := time.Since(t)
	if d < 0 { d = -d }
	return fmt.Sprintf("%s ago (%s)", niceDuration(t), t.Format("2006-01-02 15:04"))
}

func arenaDiskInfo(path string) (int, int64, int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		total := int64(stat.Blocks) * int64(stat.Bsize)
		avail := int64(stat.Bavail) * int64(stat.Bsize)
		used := total - avail
		if total > 0 { return int(float64(used) / float64(total) * 100), used, total }
	}
	return 0, 0, 0
}

func totalDisk(path string) uint64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		return uint64(stat.Blocks) * uint64(stat.Bsize)
	}
	return 0
}


func niceDuration(t time.Time) string {
	d := time.Since(t)
	if d < 0 { d = -d }
	switch {
	case d < time.Minute: return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour: return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour: return fmt.Sprintf("%dh", int(d.Hours()))
	default: return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func niceSize(n int64) string {
	suf := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for i < len(suf)-1 && f >= 995 { f /= 1024; i++ }
	if f < 10 { return fmt.Sprintf("%.1f %s", f, suf[i]) }
	return fmt.Sprintf("%.0f %s", f, suf[i])
}

func sysUptime() (string, string) {
	d, _ := os.ReadFile("/proc/uptime")
	var secs float64
	fmt.Sscanf(string(d), "%f", &secs)
	start := time.Now().Add(-time.Duration(secs) * time.Second)
	return niceDuration(start), start.Format("2006-01-02 15:04")
}

func sysMemInfo() (pct int, used, total int64) {
	d, _ := os.ReadFile("/proc/meminfo")
	var totalKB, availKB int64
	for _, line := range strings.Split(string(d), "\n") {
		var kb int64
		if strings.HasPrefix(line, "MemTotal:") { fmt.Sscanf(strings.Fields(line)[1], "%d", &kb); totalKB = kb }
		if strings.HasPrefix(line, "MemAvailable:") { fmt.Sscanf(strings.Fields(line)[1], "%d", &kb); availKB = kb }
	}
	total = totalKB * 1024
	used = (totalKB - availKB) * 1024
	if total > 0 { pct = int(float64(used) / float64(total) * 100) }
	return
}

func sysDiskInfo() (pct int, used, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		total = int64(stat.Blocks) * int64(stat.Bsize)
		avail := int64(stat.Bavail) * int64(stat.Bsize)
		used = total - avail
		if total > 0 { pct = int(float64(used) / float64(total) * 100) }
	}
	return
}

func cpuPercent() int {
	d, _ := os.ReadFile("/proc/stat")
	var user, nice, system, idle uint64
	fmt.Sscanf(string(d), "cpu %d %d %d %d", &user, &nice, &system, &idle)
	total := user + nice + system + idle
	if total > 0 { return int(100 - (float64(idle)/float64(total))*100) }
	return 0
}

func totalMem() uint64 {
	d, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(d), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			var kb uint64
			fmt.Sscanf(strings.Fields(line)[1], "%d", &kb)
			return kb * 1024
		}
	}
	return 0
}

func memClass(pct int) string {
	if pct > 80 { return "critical" }
	if pct > 50 { return "warning" }
	return ""
}

func readProcFile(path, key string) string {
	d, _ := os.ReadFile(path)
	if key == "" { return strings.TrimSpace(string(d)) }
	for _, line := range strings.Split(string(d), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.Trim(strings.TrimPrefix(line, key+"="), "\"")
		}
	}
	return ""
}


