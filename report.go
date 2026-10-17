package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// Các ngưỡng nhận diện điểm nghẽn hệ thống (knee point / saturation point).
// Khi một bước kiểm thử vượt qua một trong các ngưỡng này, bước đó sẽ bị đánh dấu là quá tải (KNEE)
// và loại khỏi tập dữ liệu dùng để tính trung vị chỉ số C:
//   - kneeAchievedRatio: Tỷ lệ RPS hoàn thành so với RPS yêu cầu (< 90% nghĩa là hàng đợi bắt đầu ứ đọng).
//   - kneeAcceptQueue: Số lượng kết nối chờ xử lý trong TCP accept queue (>= 1 nghĩa là worker không kịp accept).
//   - kneeAcceptSamples: Số mẫu quan sát thấy accept queue >= kneeAcceptQueue (>= 3 mẫu để tránh false knee do cold-start/nhiễu 1 nhịp).
//   - kneeHealthMs: Độ trễ phản hồi health check (> 1000ms là dấu hiệu event loop bị tắc nghẽn nghiêm trọng).
//   - kneeThrottlePct: Tỷ lệ chu kỳ CPU bị bóp nghẽn bởi CFS quota (> 5% nghĩa là đã chạm trần quota được cấp).
const (
	kneeAchievedRatio = 0.90   // [Tạo tải] Throughput thực tế đạt được < 90% mức yêu cầu
	kneeAcceptQueue   = 1      // [Hệ điều hành] Hàng đợi kết nối accept queue > 0
	kneeAcceptSamples = 3      // [Hệ điều hành] Số mẫu tối thiểu thấy accept queue > 0 (chống false knee 1 mẫu thoáng qua)
	kneeHealthMs      = 1000.0 // [Event loop] Độ trễ endpoint /health/live vượt quá 1 giây
	kneeThrottlePct   = 5.0    // [cgroup] Tỷ lệ chu kỳ CPU bị bóp nghẽn vượt quá 5%
)

// RampRecord đại diện cho một dòng dữ liệu trong file ramp.jsonl (run_meta, step_start, step_end, req).
type RampRecord struct {
	Type        string   `json:"type"`
	RPS         float64  `json:"rps"`
	T           float64  `json:"t"`
	StepSeconds *float64 `json:"step_seconds"`
	TStart      *float64 `json:"t_start"`
	Ok          bool     `json:"ok"`
	TtftMs      *float64 `json:"ttft_ms"`
	TotalMs     *float64 `json:"total_ms"`
	Status      *int     `json:"status"`
	Err         *string  `json:"err"`
}

// ProbeTCP chứa thông tin trạng thái kết nối TCP và hàng đợi lắng nghe trong mẫu probe.
type ProbeTCP struct {
	AcceptQueue *int           `json:"accept_queue"`
	BacklogMax  *int           `json:"backlog_max"`
	States      map[string]int `json:"states"`
}

// ProbeWorkers chứa thông tin tài nguyên tiến trình worker trong mẫu probe.
type ProbeWorkers struct {
	NProcs     *int `json:"n_procs"`
	NWorkers   *int `json:"n_workers"`
	FDsTotal   *int `json:"fds_total"`
	FDsMax     *int `json:"fds_max"`
	ThreadsMax *int `json:"threads_max"`
}

// ProbeRecord đại diện cho một dòng dữ liệu trong file probe.jsonl (probe_meta hoặc mẫu đo định kỳ).
type ProbeRecord struct {
	Type                      string        `json:"type"`
	T                         *float64      `json:"t"`
	CPUQuotaCores             *float64      `json:"cpu_quota_cores"`
	NprocVisible              *int          `json:"nproc_visible"`
	DefaultExecutorMaxWorkers *int          `json:"default_executor_max_workers"`
	UsageUsec                 *float64      `json:"usage_usec"`
	NrPeriods                 *float64      `json:"nr_periods"`
	NrThrottled               *float64      `json:"nr_throttled"`
	ThrottledUsec             *float64      `json:"throttled_usec"`
	MemBytes                  *float64      `json:"mem_bytes"`
	HealthMs                  *float64      `json:"health_ms"`
	HealthCode                *int          `json:"health_code"`
	TCP                       *ProbeTCP     `json:"tcp"`
	Workers                   *ProbeWorkers `json:"workers"`
}

// StepWindow lưu thông tin một bước kiểm thử (mức RPS, mốc thời gian và danh sách request thuộc bước đó).
type StepWindow struct {
	RPS     float64
	T0      float64
	T1      float64
	LaunchS float64
	Reqs    []RampRecord
}

// EvalRow lưu kết quả đánh giá của từng bậc RPS để tổng hợp phần kết luận.
type EvalRow struct {
	RPS      float64
	Achieved float64
	CMs      *float64
	Ok       bool
}

func loadRamp(path string) ([]RampRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []RampRecord
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r RampRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func loadProbe(path string) ([]ProbeRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []ProbeRecord
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r ProbeRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// pct tính phân vị thứ p theo phương pháp nearest-rank (chọn phần tử thực tế trong danh sách).
func pct(xs []float64, p float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	k := int(math.RoundToEven((p / 100.0) * float64(len(s)-1)))
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	val := s[k]
	return &val
}

// median tính trung vị của danh sách số thực (tương đương statistics.median của Python).
func median(xs []float64) float64 {
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2.0
}

// fmtNum định dạng số thực hiển thị trên bảng kết quả (có dấu phẩy ngăn cách hàng nghìn).
// Trả về "-" nếu không có dữ liệu (nil).
func fmtNum(v *float64, nd int) string {
	if v == nil {
		return "-"
	}
	raw := fmt.Sprintf("%.*f", nd, *v)
	parts := strings.SplitN(raw, ".", 2)
	intPart := parts[0]
	sign := ""
	if strings.HasPrefix(intPart, "-") {
		sign = "-"
		intPart = intPart[1:]
	}
	var buf strings.Builder
	n := len(intPart)
	for i, ch := range intPart {
		if i > 0 && (n-i)%3 == 0 {
			buf.WriteByte(',')
		}
		buf.WriteRune(ch)
	}
	if len(parts) == 2 {
		return sign + buf.String() + "." + parts[1]
	}
	return sign + buf.String()
}

// buildWindows phân tách các bản ghi từ ramp.jsonl thành các khung thời gian tương ứng với từng mức RPS.
func buildWindows(ramp []RampRecord, stepSeconds *float64) []StepWindow {
	var out []StepWindow
	var cur *StepWindow
	var pendingWithoutTStart []RampRecord

	for _, r := range ramp {
		switch r.Type {
		case "step_start":
			cur = &StepWindow{
				RPS: r.RPS,
				T0:  r.T,
			}
			pendingWithoutTStart = nil
		case "step_end":
			if cur != nil {
				cur.T1 = r.T
				if stepSeconds != nil && *stepSeconds > 0 {
					cur.LaunchS = *stepSeconds
				} else {
					cur.LaunchS = cur.T1 - cur.T0
				}
				cur.Reqs = append(cur.Reqs, pendingWithoutTStart...)
				out = append(out, *cur)
				cur = nil
				pendingWithoutTStart = nil
			}
		case "req":
			if r.TStart == nil && cur != nil {
				pendingWithoutTStart = append(pendingWithoutTStart, r)
			}
		}
	}

	// Gán các request vào bước kiểm thử dựa theo thời điểm bắt đầu gửi (t_start)
	for _, r := range ramp {
		if r.Type != "req" || r.TStart == nil {
			continue
		}
		for i := range out {
			if out[i].T0 <= *r.TStart && *r.TStart <= out[i].T1 {
				out[i].Reqs = append(out[i].Reqs, r)
				break
			}
		}
	}

	return out
}

// probeSlice lọc các mẫu dữ liệu probe rơi vào khoảng thời gian [t0, t1] (bỏ qua bản ghi metadata).
func probeSlice(probe []ProbeRecord, t0, t1 float64) []ProbeRecord {
	var out []ProbeRecord
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		t := 0.0
		if s.T != nil {
			t = *s.T
		}
		if t0 <= t && t <= t1 {
			out = append(out, s)
		}
	}
	return out
}

// delta tính độ chênh lệch (giá trị cuối trừ giá trị đầu) của một bộ đếm tích lũy trong khoảng thời gian đo.
func delta(samples []ProbeRecord, getter func(ProbeRecord) *float64) *float64 {
	var vals []float64
	for _, s := range samples {
		if v := getter(s); v != nil {
			vals = append(vals, *v)
		}
	}
	if len(vals) < 2 {
		return nil
	}
	d := vals[len(vals)-1] - vals[0]
	return &d
}

// baselineCPU tính mức tiêu thụ CPU nền khi chưa có tải:
// trả về (số core, số mẫu, độ dài khoảng thời gian đo tính bằng giây).
func baselineCPU(probe []ProbeRecord, firstT0 float64) (*float64, int, float64) {
	var pre []ProbeRecord
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		t := 0.0
		if s.T != nil {
			t = *s.T
		}
		if t < firstT0 {
			pre = append(pre, s)
		}
	}
	if len(pre) < 3 {
		return nil, len(pre), 0.0
	}
	tFirst := 0.0
	if pre[0].T != nil {
		tFirst = *pre[0].T
	}
	tLast := 0.0
	if pre[len(pre)-1].T != nil {
		tLast = *pre[len(pre)-1].T
	}
	spanS := tLast - tFirst
	d := delta(pre, func(s ProbeRecord) *float64 { return s.UsageUsec })
	if d == nil || *d == 0 || spanS <= 0 {
		return nil, len(pre), math.Max(0.0, spanS)
	}
	val := *d / (spanS * 1_000_000.0)
	return &val, len(pre), spanS
}

func formatNoisySlice(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		if x == math.Trunc(x) {
			parts[i] = fmt.Sprintf("%.1f", x)
		} else {
			parts[i] = fmt.Sprintf("%g", x)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func main() {
	rampPath := flag.String("ramp", "", "đường dẫn file JSONL kết quả phát tải (bắt buộc)")
	probePath := flag.String("probe", "", "đường dẫn file JSONL chỉ số hệ thống từ probe")
	flag.Parse()

	if *rampPath == "" {
		fmt.Fprintln(os.Stderr, "ERROR: bắt buộc truyền cờ --ramp <file-ramp.jsonl>")
		flag.Usage()
		os.Exit(2)
	}

	ramp, err := loadRamp(*rampPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: không đọc được file ramp %s: %v\n", *rampPath, err)
		os.Exit(1)
	}

	var probe []ProbeRecord
	if *probePath != "" {
		probe, err = loadProbe(*probePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: không đọc được file probe %s: %v\n", *probePath, err)
			os.Exit(1)
		}
	}

	var meta *ProbeRecord
	for i := range probe {
		if probe[i].Type == "probe_meta" {
			meta = &probe[i]
			break
		}
	}

	var quota *float64
	if meta != nil {
		quota = meta.CPUQuotaCores
	}

	var stepSeconds *float64
	for i := range ramp {
		if ramp[i].Type == "run_meta" {
			stepSeconds = ramp[i].StepSeconds
			break
		}
	}

	wins := buildWindows(ramp, stepSeconds)
	if len(wins) == 0 {
		fmt.Println("Không tìm thấy bậc nào trong file ramp.")
		os.Exit(1)
	}

	var (
		baseline *float64
		baseN    int
		baseSpan float64
	)
	if len(probe) > 0 {
		baseline, baseN, baseSpan = baselineCPU(probe, wins[0].T0)
	}

	workersSeen := 0
	for _, s := range probe {
		if s.Type != "probe_meta" && s.Workers != nil && s.Workers.NWorkers != nil {
			if *s.Workers.NWorkers > workersSeen {
				workersSeen = *s.Workers.NWorkers
			}
		}
	}

	fmt.Println(strings.Repeat("=", 132))
	fmt.Println("CALIBRATE - KẾT QUẢ")
	fmt.Println(strings.Repeat("=", 132))
	if meta != nil {
		if quota != nil {
			if *quota == math.Trunc(*quota) {
				fmt.Printf("cpu_quota_cores : %.1f\n", *quota)
			} else {
				fmt.Printf("cpu_quota_cores : %g\n", *quota)
			}
		} else {
			fmt.Println("cpu_quota_cores : không giới hạn")
		}
		nprocStr := "None"
		if meta.NprocVisible != nil {
			nprocStr = fmt.Sprintf("%d", *meta.NprocVisible)
		}
		fmt.Printf("nproc nhìn thấy : %s (core của NODE, không phải quota)\n", nprocStr)
		execStr := "None"
		if meta.DefaultExecutorMaxWorkers != nil {
			execStr = fmt.Sprintf("%d", *meta.DefaultExecutorMaxWorkers)
		}
		fmt.Printf("executor threads: %s / worker\n", execStr)
		if workersSeen > 0 {
			fmt.Printf("workers quan sát: %d worker\n", workersSeen)
		}
	}
	if baseline != nil {
		fmt.Printf("CPU nền (không tải)    : %.3f core (%d mẫu / %.0fs)\n", *baseline, baseN, baseSpan)
		if baseSpan < 90 {
			fmt.Printf("  CẢNH BÁO: cửa sổ nền chỉ %.0fs. Các tác vụ nền (background sync/cron) thường có chu kỳ 30-45s nên\n"+
				"  cửa sổ ngắn làm baseline nhảy giữa các lần chạy. Lần sau chờ 90-120s trước khi bắn tải.\n", baseSpan)
		}
	} else if len(probe) > 0 {
		fmt.Printf("CPU nền                : KHÔNG đo được (%d mẫu trước bậc đầu, cần >= 3)\n", baseN)
		fmt.Println("  -> probe bật muộn hơn ramp? C dưới đây chưa trừ nền nên bị thổi phồng.")
	}
	fmt.Println()

	cols := fmt.Sprintf("%6s %7s %5s %4s %8s %8s %6s %7s %6s %6s %8s %6s %6s %5s  verdict",
		"offer", "achiev", "ok", "err", "ttftP50", "ttftP95",
		"cores", "C_ms", "thr%", "acptQ", "hlthP99", "estab", "fdMax", "thrd")
	fmt.Println(cols)
	fmt.Println(strings.Repeat("-", 132))

	var rows []EvalRow
	var noisy []float64

	for _, w := range wins {
		dur := math.Max(w.LaunchS, 1e-9)
		reqs := w.Reqs
		var okReqs []RampRecord
		var ttfts []float64
		for _, r := range reqs {
			if r.Ok {
				okReqs = append(okReqs, r)
				if r.TtftMs != nil {
					ttfts = append(ttfts, *r.TtftMs)
				}
			}
		}

		s := probeSlice(probe, w.T0, w.T1)

		var (
			cores, cMs, thrPct                           *float64
			acceptq, healthP99, estab, fdmax, thrd       *float64
			acptSamples                                  int
		)

		if len(s) > 0 {
			tFirst := 0.0
			if s[0].T != nil {
				tFirst = *s[0].T
			}
			tLast := 0.0
			if s[len(s)-1].T != nil {
				tLast = *s[len(s)-1].T
			}
			spanUsec := (tLast - tFirst) * 1_000_000.0
			dUsage := delta(s, func(x ProbeRecord) *float64 { return x.UsageUsec })
			if dUsage != nil && spanUsec > 0 {
				vCores := *dUsage / spanUsec
				cores = &vCores
				baseUsec := 0.0
				if baseline != nil {
					baseUsec = *baseline * spanUsec
				}
				net := *dUsage - baseUsec
				if len(okReqs) > 0 && net > 0 {
					vCMs := net / 1000.0 / float64(len(okReqs))
					cMs = &vCMs
				} else if len(okReqs) > 0 {
					noisy = append(noisy, w.RPS)
				}
			}

			dThr := delta(s, func(x ProbeRecord) *float64 { return x.NrThrottled })
			dPer := delta(s, func(x ProbeRecord) *float64 { return x.NrPeriods })
			if dThr != nil && dPer != nil && *dPer != 0 {
				vThr := 100.0 * (*dThr) / (*dPer)
				thrPct = &vThr
			}

			maxAcpt := 0
			maxEstab := 0
			maxFD := 0
			maxThrd := 0
			var healthVals []float64

			for _, x := range s {
				aq := 0
				if x.TCP != nil && x.TCP.AcceptQueue != nil {
					aq = *x.TCP.AcceptQueue
				}
				if aq > maxAcpt {
					maxAcpt = aq
				}
				if aq >= kneeAcceptQueue {
					acptSamples++
				}

				if x.HealthMs != nil {
					healthVals = append(healthVals, *x.HealthMs)
				}

				es := 0
				if x.TCP != nil && x.TCP.States != nil {
					es = x.TCP.States["ESTAB"]
				}
				if es > maxEstab {
					maxEstab = es
				}

				fd := 0
				if x.Workers != nil && x.Workers.FDsMax != nil {
					fd = *x.Workers.FDsMax
				}
				if fd > maxFD {
					maxFD = fd
				}

				th := 0
				if x.Workers != nil && x.Workers.ThreadsMax != nil {
					th = *x.Workers.ThreadsMax
				}
				if th > maxThrd {
					maxThrd = th
				}
			}

			vAcpt := float64(maxAcpt)
			acceptq = &vAcpt
			healthP99 = pct(healthVals, 99)
			vEstab := float64(maxEstab)
			estab = &vEstab
			vFD := float64(maxFD)
			fdmax = &vFD
			vThrd := float64(maxThrd)
			thrd = &vThrd
		}

		achieved := float64(len(okReqs)) / dur
		var reasons []string
		if achieved < kneeAchievedRatio*w.RPS {
			reasons = append(reasons, "achieved<offered")
		}
		if acptSamples >= kneeAcceptSamples {
			reasons = append(reasons, fmt.Sprintf("acptQ %d mẫu", acptSamples))
		}
		if healthP99 != nil && *healthP99 > kneeHealthMs {
			reasons = append(reasons, "health chậm")
		}
		if thrPct != nil && *thrPct > kneeThrottlePct {
			reasons = append(reasons, "throttled")
		}
		for _, r := range reqs {
			if r.Err != nil && *r.Err == "abandoned" {
				reasons = append(reasons, "abandoned")
				break
			}
		}

		verdict := "OK"
		if len(reasons) > 0 {
			verdict = "KNEE: " + strings.Join(reasons, ", ")
		}

		rows = append(rows, EvalRow{
			RPS:      w.RPS,
			Achieved: achieved,
			CMs:      cMs,
			Ok:       len(reasons) == 0,
		})

		fmt.Printf("%6.1f %7.2f %5d %4d %8s %8s %6s %7s %6s %6s %8s %6s %6s %5s  %s\n",
			w.RPS, achieved, len(okReqs), len(reqs)-len(okReqs),
			fmtNum(pct(ttfts, 50), 0), fmtNum(pct(ttfts, 95), 0),
			fmtNum(cores, 2), fmtNum(cMs, 1), fmtNum(thrPct, 1), fmtNum(acceptq, 0),
			fmtNum(healthP99, 0), fmtNum(estab, 0), fmtNum(fdmax, 0), fmtNum(thrd, 0), verdict)
	}

	fmt.Println(strings.Repeat("-", 132))

	var healthyC []float64
	var anyOk bool
	var knees []EvalRow
	var lastOk []EvalRow

	for _, r := range rows {
		if r.Ok {
			anyOk = true
			lastOk = append(lastOk, r)
			if r.CMs != nil && *r.CMs != 0 {
				healthyC = append(healthyC, *r.CMs)
			}
		} else {
			knees = append(knees, r)
		}
	}

	fmt.Println()
	fmt.Println("KẾT LUẬN")
	if len(healthyC) > 0 {
		c := median(healthyC)
		fmt.Printf("  C (trung vị các bậc còn khoẻ)   : %.1f ms CPU / request\n", c)
		fmt.Printf("  Trần lý thuyết 1 worker (1 core) : %.1f req/s\n", 1000.0/c)
		if workersSeen > 1 {
			fmt.Printf("  Trần lý thuyết %d worker (%d core): %.1f req/s\n", workersSeen, workersSeen, float64(workersSeen)*1000.0/c)
		}
		if quota != nil && *quota > 0 {
			fmt.Printf("  Trần lý thuyết pod (%g core)      : %.1f req/s\n", *quota, (*quota)*1000.0/c)
			recWorkers := int(math.RoundToEven(*quota))
			if recWorkers < 1 {
				recWorkers = 1
			}
			fmt.Printf("  Số worker hợp lý                 : %d (= quota)\n", recWorkers)
			if workersSeen > 0 && workersSeen < int(math.RoundToEven(*quota)) {
				fmt.Printf("  CẢNH BÁO: Số worker đang chạy (%d) thấp hơn CPU quota (%g core). Cần nâng lên %d worker để khai thác tối đa năng lực pod.\n",
					workersSeen, *quota, recWorkers)
			}
		}
	} else if !anyOk {
		fmt.Println("  Chưa có bậc nào 'OK' -> hạ dải --steps xuống rồi đo lại.")
	} else {
		fmt.Println("  Có bậc còn khoẻ nhưng KHÔNG tính được C -> thiếu --probe, hoặc probe không")
		fmt.Println("  phủ cùng khoảng thời gian với ramp (xem cột cores/C_ms trống).")
	}

	if len(noisy) > 0 {
		fmt.Printf("  Không đo được C ở bậc %s: CPU lúc tải không cao hơn CPU nền. Nguyên nhân\n"+
			"  thường gặp: cgroup chứa cả tiến trình khác ngoài app, hoặc bậc RPS quá nhẹ.\n", formatNoisySlice(noisy))
	}

	if len(lastOk) > 0 {
		lastOkRPS := lastOk[len(lastOk)-1].RPS
		fmt.Printf("  Bậc cuối còn khoẻ                : %g req/s -> SLO\n", lastOkRPS)
		var subsequentKnees []EvalRow
		for _, r := range rows {
			if !r.Ok && r.RPS > lastOkRPS {
				subsequentKnees = append(subsequentKnees, r)
			}
		}
		if len(subsequentKnees) > 0 {
			fmt.Printf("  Điểm gãy quan sát được           : %g req/s offered\n", subsequentKnees[0].RPS)
		} else {
			fmt.Println("  Chưa chạm điểm gãy sau bậc khoẻ cuối cùng - nâng dải --steps lên và đo tiếp.")
		}
	} else if len(knees) > 0 {
		fmt.Printf("  Điểm gãy quan sát được           : %g req/s offered\n", knees[0].RPS)
	} else {
		fmt.Println("  Chưa chạm điểm gãy - nâng dải --steps lên và đo tiếp.")
	}
	fmt.Println()
	fmt.Println("  Chênh giữa trần lý thuyết và điểm gãy quan sát được là phần bị mất vì GIL,")
	fmt.Println("  CFS throttling và tranh chấp thread. Chênh lớn -> xem cột thr% và thrd trước tiên.")
}
