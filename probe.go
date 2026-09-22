package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// TCPStates maps hex state codes from /proc/net/tcp to human-readable state names.
var tcpStates = map[string]string{
	"01": "ESTAB",
	"02": "SYN_SENT",
	"03": "SYN_RECV",
	"04": "FIN_WAIT1",
	"05": "FIN_WAIT2",
	"06": "TIME_WAIT",
	"07": "CLOSE",
	"08": "CLOSE_WAIT",
	"09": "LAST_ACK",
	"0A": "LISTEN",
	"0B": "CLOSING",
}

const cg2Path = "/sys/fs/cgroup"

// readFile reads the full content of a file, returning an empty string on error.
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// cpuQuotaCores calculates the number of CPU cores allocated from cgroup quota.
// Returns nil if unlimited.
func cpuQuotaCores() *float64 {
	// cgroup v2: /sys/fs/cgroup/cpu.max ("<quota> <period>")
	v2 := readFile(filepath.Join(cg2Path, "cpu.max"))
	if v2 != "" {
		parts := strings.Fields(v2)
		if len(parts) >= 2 {
			if parts[0] == "max" {
				return nil
			}
			quota, err1 := strconv.ParseFloat(parts[0], 64)
			period, err2 := strconv.ParseFloat(parts[1], 64)
			if err1 == nil && err2 == nil && period > 0 {
				val := quota / period
				return &val
			}
		}
	}

	// cgroup v1: /sys/fs/cgroup/cpu/cpu.cfs_quota_us and cpu.cfs_period_us
	quotaStr := readFile(filepath.Join(cg2Path, "cpu/cpu.cfs_quota_us"))
	periodStr := readFile(filepath.Join(cg2Path, "cpu/cpu.cfs_period_us"))
	if quotaStr != "" && periodStr != "" {
		q, err1 := strconv.ParseFloat(strings.TrimSpace(quotaStr), 64)
		p, err2 := strconv.ParseFloat(strings.TrimSpace(periodStr), 64)
		if err1 == nil && err2 == nil && p > 0 {
			if q < 0 {
				return nil
			}
			val := q / p
			return &val
		}
	}

	return nil
}

// cpuSample gathers cumulative CPU counters from cgroup: usage_usec, nr_periods, nr_throttled, throttled_usec.
func cpuSample() (usageUsec, nrPeriods, nrThrottled, throttledUsec *int64) {
	// Try cgroup v2: /sys/fs/cgroup/cpu.stat
	v2 := readFile(filepath.Join(cg2Path, "cpu.stat"))
	if v2 != "" && strings.Contains(v2, "usage_usec") {
		scanner := bufio.NewScanner(strings.NewReader(v2))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 {
				val, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					continue
				}
				switch fields[0] {
				case "usage_usec":
					v := val
					usageUsec = &v
				case "nr_periods":
					v := val
					nrPeriods = &v
				case "nr_throttled":
					v := val
					nrThrottled = &v
				case "throttled_usec":
					v := val
					throttledUsec = &v
				}
			}
		}
		return
	}

	// Try cgroup v1: /sys/fs/cgroup/cpuacct/cpuacct.usage (nanoseconds -> microseconds)
	acct := readFile(filepath.Join(cg2Path, "cpuacct/cpuacct.usage"))
	if acct != "" {
		val, err := strconv.ParseInt(strings.TrimSpace(acct), 10, 64)
		if err == nil {
			v := val / 1000
			usageUsec = &v
		}
	}

	// cgroup v1: /sys/fs/cgroup/cpu/cpu.stat
	v1 := readFile(filepath.Join(cg2Path, "cpu/cpu.stat"))
	if v1 != "" {
		scanner := bufio.NewScanner(strings.NewReader(v1))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 {
				val, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					continue
				}
				switch fields[0] {
				case "nr_periods":
					v := val
					nrPeriods = &v
				case "nr_throttled":
					v := val
					nrThrottled = &v
				case "throttled_time": // nanoseconds -> microseconds
					v := val / 1000
					throttledUsec = &v
				}
			}
		}
	}

	return
}

// memBytes gets current memory usage in bytes from cgroup.
func memBytes() *int64 {
	paths := []string{
		filepath.Join(cg2Path, "memory.current"),
		filepath.Join(cg2Path, "memory/memory.usage_in_bytes"),
	}
	for _, p := range paths {
		text := readFile(p)
		if text != "" {
			val, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if err == nil {
				return &val
			}
		}
	}
	return nil
}

// parseProcNetTCP parses /proc/net/tcp or tcp6 for the specified port.
func parseProcNetTCP(path string, port int) (map[string]int, map[string]int) {
	counts := make(map[string]int)
	listen := make(map[string]int)

	content := readFile(path)
	if content == "" {
		return counts, listen
	}

	lines := strings.Split(content, "\n")
	if len(lines) <= 1 {
		return counts, listen
	}

	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		lparts := strings.Split(fields[1], ":")
		rparts := strings.Split(fields[2], ":")
		if len(lparts) < 2 || len(rparts) < 2 {
			continue
		}

		lport64, err1 := strconv.ParseInt(lparts[len(lparts)-1], 16, 64)
		rport64, err2 := strconv.ParseInt(rparts[len(rparts)-1], 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}

		lport := int(lport64)
		rport := int(rport64)
		if lport != port && rport != port {
			continue
		}

		stCode := strings.ToUpper(fields[3])
		st, ok := tcpStates[stCode]
		if !ok {
			st = stCode
		}

		key := st
		if lport != port {
			key = st + "_out"
		}
		counts[key]++

		if st == "LISTEN" && lport == port {
			qParts := strings.Split(fields[4], ":")
			if len(qParts) == 2 {
				tx, errTx := strconv.ParseInt(qParts[0], 16, 64)
				rx, errRx := strconv.ParseInt(qParts[1], 16, 64)
				if errTx == nil && errRx == nil {
					listen["accept_queue"] = int(rx)
					listen["backlog_max"] = int(tx)
				}
			}
		}
	}

	return counts, listen
}

// tcpSample aggregates TCP socket states and listen queue info for IPv4 and IPv6.
func tcpSample(port int) map[string]any {
	counts := make(map[string]int)
	listen := make(map[string]int)

	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		c, ls := parseProcNetTCP(path, port)
		for k, v := range c {
			counts[k] += v
		}
		for k, v := range ls {
			listen[k] = v
		}
	}

	out := map[string]any{
		"states": counts,
	}
	for k, v := range listen {
		out[k] = v
	}
	return out
}

type procRaw struct {
	pid      int
	ppid     int
	cmd      string
	threads  *int
	fds      int
	cpuTicks int64
}

// ProcInfo represents serialized process metrics.
type ProcInfo struct {
	PID      int    `json:"pid"`
	Role     string `json:"role"`
	FDs      int    `json:"fds"`
	Threads  *int   `json:"threads"`
	CPUTicks int64  `json:"cpu_ticks"`
}

// workerSample inspects target application processes from /proc.
func workerSample(pattern string) map[string]any {
	allProcs := make(map[int]procRaw)
	myPID := os.Getpid()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return map[string]any{
			"n_procs":     0,
			"n_workers":   0,
			"fds_total":   0,
			"fds_max":     nil,
			"threads_max": nil,
			"procs":       []ProcInfo{},
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == myPID {
			continue
		}

		stat := readFile(fmt.Sprintf("/proc/%d/stat", pid))
		if stat == "" {
			continue
		}
		rparen := strings.LastIndex(stat, ")")
		if rparen < 0 || rparen+2 >= len(stat) {
			continue
		}
		tail := strings.Fields(stat[rparen+2:])
		if len(tail) < 13 {
			continue
		}

		ppid, _ := strconv.Atoi(tail[1])
		utime, _ := strconv.ParseInt(tail[11], 10, 64)
		stime, _ := strconv.ParseInt(tail[12], 10, 64)
		ticks := utime + stime

		cmdRaw := readFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		cmd := strings.TrimSpace(strings.ReplaceAll(cmdRaw, "\x00", " "))
		if cmd == "" {
			// Fallback to comm in stat: e.g. "(app)"
			lparen := strings.Index(stat, "(")
			if lparen >= 0 && lparen < rparen {
				cmd = stat[lparen+1 : rparen]
			}
		}

		var threads *int
		status := readFile(fmt.Sprintf("/proc/%d/status", pid))
		if status != "" {
			scanner := bufio.NewScanner(strings.NewReader(status))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "Threads:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						th, err := strconv.Atoi(fields[1])
						if err == nil {
							threads = &th
						}
					}
					break
				}
			}
		}

		fds := -1
		fdEntries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err == nil {
			fds = len(fdEntries)
		}

		allProcs[pid] = procRaw{
			pid:      pid,
			ppid:     ppid,
			cmd:      cmd,
			threads:  threads,
			fds:      fds,
			cpuTicks: ticks,
		}
	}

	var masterPIDs = make(map[int]bool)
	var keywords []string
	for _, kw := range strings.Split(pattern, "|") {
		kw = strings.TrimSpace(strings.ToLower(kw))
		if kw != "" {
			keywords = append(keywords, kw)
		}
	}

	if len(keywords) > 0 {
		for pid, p := range allProcs {
			cmdLower := strings.ToLower(p.cmd)
			for _, kw := range keywords {
				if strings.Contains(cmdLower, kw) {
					masterPIDs[pid] = true
					break
				}
			}
		}
	} else {
		// Auto-detection mode (no pattern specified):
		// 1. In a standard container, PID 1 is the entrypoint (or tini/dumb-init).
		// Check if PID 1 is NOT a system init (systemd/init) and not a shell.
		p1, hasP1 := allProcs[1]
		isSystemInit := false
		if hasP1 {
			cLower := strings.ToLower(p1.cmd)
			if cLower == "systemd" || cLower == "init" || strings.HasPrefix(cLower, "/sbin/init") || cLower == "/pause" {
				isSystemInit = true
			}
		}

		if hasP1 && !isSystemInit {
			// Inside container namespace: target PID 1 (and its descendants will be added below)
			masterPIDs[1] = true
		} else {
			// Fallback: check for common app binary names or servers
			fallbackKws := []string{"uvicorn", "gunicorn", "granian", "hypercorn", "fastapi", "server", "app", "main"}
			for pid, p := range allProcs {
				cmdLower := strings.ToLower(p.cmd)
				for _, kw := range fallbackKws {
					if strings.Contains(cmdLower, kw) {
						masterPIDs[pid] = true
						break
					}
				}
			}
		}
	}

	// Expand target PIDs to all descendants
	targetPIDs := make(map[int]bool)
	for pid := range masterPIDs {
		targetPIDs[pid] = true
	}

	changed := true
	for changed {
		changed = false
		for pid, p := range allProcs {
			if !targetPIDs[pid] && targetPIDs[p.ppid] {
				targetPIDs[pid] = true
				changed = true
			}
		}
	}

	// Filter out multiprocessing helpers
	helperMarkers := []string{"multiprocessing.resource_tracker", "multiprocessing.forkserver"}
	for pid := range targetPIDs {
		cmd := allProcs[pid].cmd
		for _, m := range helperMarkers {
			if strings.Contains(cmd, m) {
				delete(targetPIDs, pid)
				break
			}
		}
	}

	var targetProcs []procRaw
	for pid := range targetPIDs {
		if p, ok := allProcs[pid]; ok {
			targetProcs = append(targetProcs, p)
		}
	}

	parentPIDs := make(map[int]bool)
	for _, p := range targetProcs {
		if targetPIDs[p.ppid] {
			parentPIDs[p.ppid] = true
		}
	}

	var workers []procRaw
	var masters []procRaw
	var procs []ProcInfo

	for _, p := range targetProcs {
		role := "worker"
		if parentPIDs[p.pid] {
			role = "master"
		}
		procs = append(procs, ProcInfo{
			PID:      p.pid,
			Role:     role,
			FDs:      p.fds,
			Threads:  p.threads,
			CPUTicks: p.cpuTicks,
		})
		if role == "worker" {
			workers = append(workers, p)
		} else {
			masters = append(masters, p)
		}
	}

	sort.Slice(procs, func(i, j int) bool {
		return procs[i].PID < procs[j].PID
	})

	activeWorkers := workers
	if len(activeWorkers) == 0 {
		activeWorkers = targetProcs
	}

	var workerFDs []int
	var workerThreads []int
	for _, p := range activeWorkers {
		if p.fds >= 0 {
			workerFDs = append(workerFDs, p.fds)
		}
		if p.threads != nil {
			workerThreads = append(workerThreads, *p.threads)
		}
	}

	fdsTotal := 0
	for _, p := range targetProcs {
		if p.fds >= 0 {
			fdsTotal += p.fds
		}
	}

	var fdsMax *int
	if len(workerFDs) > 0 {
		maxVal := workerFDs[0]
		for _, v := range workerFDs[1:] {
			if v > maxVal {
				maxVal = v
			}
		}
		fdsMax = &maxVal
	}

	var threadsMax *int
	if len(workerThreads) > 0 {
		maxVal := workerThreads[0]
		for _, v := range workerThreads[1:] {
			if v > maxVal {
				maxVal = v
			}
		}
		threadsMax = &maxVal
	}

	nWorkers := len(workers)
	if nWorkers == 0 {
		nWorkers = len(targetProcs)
	}

	if procs == nil {
		procs = []ProcInfo{}
	}

	return map[string]any{
		"n_procs":     len(targetProcs),
		"n_workers":   nWorkers,
		"fds_total":   fdsTotal,
		"fds_max":     fdsMax,
		"threads_max": threadsMax,
		"procs":       procs,
	}
}

// healthSample measures HTTP GET response latency and status code.
func healthSample(url string, timeout time.Duration) (float64, *int) {
	t0 := time.Now()
	client := &http.Client{
		Timeout: timeout,
	}
	resp, err := client.Get(url)
	elapsedMs := float64(time.Since(t0).Nanoseconds()) / 1e6

	if err != nil {
		return elapsedMs, nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	code := resp.StatusCode
	return elapsedMs, &code
}

type options struct {
	out           string
	interval      float64
	port          int
	target        string
	healthTimeout float64
	noCgroup      bool
	once          bool
	procPattern   string
}

// snapshot takes a complete point-in-time metrics sample.
func snapshot(opts *options) map[string]any {
	now := float64(time.Now().UnixNano()) / 1e9
	s := map[string]any{
		"t": now,
	}

	if !opts.noCgroup {
		usageUsec, nrPeriods, nrThrottled, throttledUsec := cpuSample()
		s["usage_usec"] = usageUsec
		s["nr_periods"] = nrPeriods
		s["nr_throttled"] = nrThrottled
		s["throttled_usec"] = throttledUsec
		s["mem_bytes"] = memBytes()
		s["tcp"] = tcpSample(opts.port)
		s["workers"] = workerSample(opts.procPattern)
	}

	hURL := strings.TrimRight(opts.target, "/") + "/health/live"
	hMS, hCode := healthSample(hURL, time.Duration(opts.healthTimeout*float64(time.Second)))
	s["health_ms"] = hMS
	s["health_code"] = hCode

	return s
}

func main() {
	opts := &options{}
	flag.StringVar(&opts.out, "out", "/tmp/probe.jsonl", "đường dẫn file JSONL lưu kết quả lấy mẫu")
	flag.Float64Var(&opts.interval, "interval", 1.0, "chu kỳ lấy mẫu tính bằng giây")
	flag.IntVar(&opts.port, "port", 8000, "cổng dịch vụ TCP cần theo dõi")
	flag.StringVar(&opts.target, "target", "http://127.0.0.1:8000", "URL gốc của dịch vụ mục tiêu")
	flag.Float64Var(&opts.healthTimeout, "health-timeout", 5.0, "thời gian chờ tối đa cho request health check (giây)")
	flag.BoolVar(&opts.noCgroup, "no-cgroup", false, "chạy ngoài pod: chỉ đo health")
	flag.BoolVar(&opts.once, "once", false, "in một snapshot rồi thoát")
	flag.StringVar(&opts.procPattern, "proc-pattern", "", "từ khóa lọc tiến trình ứng dụng (để trống: tự nhận diện ứng dụng)")

	flag.Parse()

	quota := cpuQuotaCores()
	numCPU := runtime.NumCPU()
	defExecWorkers := numCPU + 4
	if defExecWorkers > 32 {
		defExecWorkers = 32
	}

	meta := map[string]any{
		"type":                         "probe_meta",
		"t":                            float64(time.Now().UnixNano()) / 1e9,
		"cpu_quota_cores":              quota,
		"nproc_visible":                numCPU,
		"default_executor_max_workers": defExecWorkers,
	}

	if opts.once {
		bMeta, _ := json.MarshalIndent(meta, "", "  ")
		fmt.Println(string(bMeta))
		bSnap, _ := json.MarshalIndent(snapshot(opts), "", "  ")
		fmt.Println(string(bSnap))
		return
	}

	outDir := filepath.Dir(opts.out)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi tạo thư mục %s: %v\n", outDir, err)
		os.Exit(1)
	}

	f, err := os.OpenFile(opts.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi mở file %s: %v\n", opts.out, err)
		os.Exit(1)
	}
	defer f.Close()

	fmt.Printf("probe -> %s (interval %.1fs). Ctrl-C để dừng.\n", opts.out, opts.interval)
	if quota != nil {
		fmt.Printf("cpu_quota_cores=%.2f  nproc_visible=%d\n", *quota, numCPU)
		if float64(numCPU) > (*quota)*2 {
			fmt.Printf("CẢNH BÁO: nproc=%d nhưng quota chỉ %.2f core. Mọi thư viện sizing theo runtime.NumCPU() đang phình quá quota.\n", numCPU, *quota)
		}
	} else {
		fmt.Printf("cpu_quota_cores=None  nproc_visible=%d\n", numCPU)
	}

	bMeta, _ := json.Marshal(meta)
	_, _ = f.Write(append(bMeta, '\n'))
	_ = f.Sync()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	intervalDur := time.Duration(opts.interval * float64(time.Second))
	nextT := time.Now()

	for {
		nextT = nextT.Add(intervalDur)
		snap := snapshot(opts)
		bSnap, err := json.Marshal(snap)
		if err == nil {
			_, _ = f.Write(append(bSnap, '\n'))
			_ = f.Sync()
		}

		sleepDur := time.Until(nextT)
		if sleepDur <= 0 {
			nextT = time.Now()
			sleepDur = 0
		}

		select {
		case <-sigChan:
			fmt.Println("\ndừng probe.")
			return
		case <-time.After(sleepDur):
		}
	}
}
