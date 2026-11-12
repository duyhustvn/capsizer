package analyzer

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// RunAnalysis phân tích toàn bộ dữ liệu từ ramp và probe, in bảng báo cáo và kết luận ra os.Stdout.
func RunAnalysis(ramp []RampRecord, probe []ProbeRecord) error {
	_, err := RunAnalysisTo(ramp, probe, os.Stdout)
	return err
}

// Analyze phân tích dữ liệu và trả về cấu trúc kết quả AnalysisReport (không in ra stdout).
func Analyze(ramp []RampRecord, probe []ProbeRecord) (*AnalysisReport, error) {
	var buf strings.Builder
	report, err := RunAnalysisTo(ramp, probe, &buf)
	if err != nil {
		return nil, err
	}
	report.ReportText = buf.String()
	return report, nil
}

// RunAnalysisTo phân tích dữ liệu và ghi kết quả dạng text vào io.Writer được chỉ định.
func RunAnalysisTo(ramp []RampRecord, probe []ProbeRecord, out io.Writer) (*AnalysisReport, error) {
	if out == nil {
		out = io.Discard
	}
	printf := func(format string, a ...interface{}) { fmt.Fprintf(out, format, a...) }
	println := func(a ...interface{}) { fmt.Fprintln(out, a...) }

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

	wins := BuildWindows(ramp, stepSeconds)
	if len(wins) == 0 {
		return nil, fmt.Errorf("không tìm thấy bậc nào trong file ramp")
	}

	var (
		baselineCPU *float64
		baseCPUN    int
		baseCPUSpan float64
	)
	var (
		baselineRAM *float64
		baseRAMN    int
	)

	if len(probe) > 0 {
		baselineCPU, baseCPUN, baseCPUSpan = BaselineCPU(probe, wins[0].T0)
		baselineRAM, baseRAMN = BaselineRAM(probe, wins[0].T0)
	}

	workersSeen := 0
	for _, s := range probe {
		if s.Type != "probe_meta" && s.Workers != nil && s.Workers.NWorkers != nil {
			if *s.Workers.NWorkers > workersSeen {
				workersSeen = *s.Workers.NWorkers
			}
		}
	}

	println(strings.Repeat("=", 149))
	println("                             CAPSIZER - KẾT QUẢ ĐỊNH CHUẨN HIỆU NĂNG & NĂNG LỰC")
	println(strings.Repeat("=", 149))
	if meta != nil {
		if quota != nil {
			if *quota == math.Trunc(*quota) {
				printf("CPU Quota Cores : %.1f core\n", *quota)
			} else {
				printf("CPU Quota Cores : %g core\n", *quota)
			}
		} else {
			println("CPU Quota Cores : không giới hạn (unlimited)")
		}
		nprocStr := "None"
		if meta.NProcVisible != nil {
			nprocStr = fmt.Sprintf("%d", *meta.NProcVisible)
		}
		printf("NProc nhìn thấy : %s (core của Node, không phải quota)\n", nprocStr)
		execStr := "None"
		if meta.DefaultExecutorMaxWorkers != nil {
			execStr = fmt.Sprintf("%d", *meta.DefaultExecutorMaxWorkers)
		}
		printf("Executor Threads: %s / worker\n", execStr)
		if workersSeen > 0 {
			printf("Workers quan sát: %d worker\n", workersSeen)
		}
	}

	if baselineCPU != nil {
		printf("CPU Nền (khi rỗi): %.3f core (%d mẫu / %.0fs)\n", *baselineCPU, baseCPUN, baseCPUSpan)
		if baseCPUSpan < 90 {
			printf("  LƯU Ý: Cửa sổ đo nền chỉ %.0fs. Cửa sổ ngắn có thể làm baseline nhảy giữa các lần chạy.\n", baseCPUSpan)
		}
	} else if len(probe) > 0 {
		printf("CPU Nền          : KHÔNG đo được (%d mẫu trước bậc đầu, cần >= 3)\n", baseCPUN)
	}

	if baselineRAM != nil {
		printf("RAM Nền (khi rỗi): %.1f MB (%d mẫu)\n", *baselineRAM, baseRAMN)
	}

	println()

	// In header bảng kết quả (có bổ sung cột RAM và Conns)
	cols := fmt.Sprintf("%6s %7s %5s %4s %8s %8s %6s %7s %7s %7s %7s %6s %6s %6s %8s %5s  verdict",
		"offer", "achiev", "ok", "err", "ttftP50", "ttftP95",
		"cores", "C_ms", "ramMB", "pkRAM", "dRAM", "thr%", "conns", "acptQ", "hlthP99", "thrd")
	println(cols)
	println(strings.Repeat("-", 149))

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

		s := ProbeSlice(probe, w.T0, w.T1)

		var (
			cores, cMs, thrPct              *float64
			acceptq, healthP99, fdmax, thrd *float64
			estabConnsP99                   *float64
			acptSamples                     int
		)

		// Phân tích RAM trong step
		memStats := StepRAM(s, baselineRAM)

		if len(s) > 0 {
			spanUsec := (s[len(s)-1].T - s[0].T) * 1_000_000.0
			dUsage := Delta(s, func(x ProbeRecord) *float64 { return x.UsageUsec })
			if dUsage != nil && spanUsec > 0 {
				vCores := *dUsage / spanUsec
				cores = &vCores
				baseUsec := 0.0
				if baselineCPU != nil {
					baseUsec = *baselineCPU * spanUsec
				}
				net := *dUsage - baseUsec
				if len(okReqs) > 0 && net > 0 {
					vCMs := net / 1000.0 / float64(len(okReqs))
					cMs = &vCMs
				} else if len(okReqs) > 0 {
					noisy = append(noisy, w.RPS)
				}
			}

			dThr := Delta(s, func(x ProbeRecord) *float64 { return x.NrThrottled })
			dPer := Delta(s, func(x ProbeRecord) *float64 { return x.NrPeriods })
			if dThr != nil && dPer != nil && *dPer != 0 {
				vThr := 100.0 * (*dThr) / (*dPer)
				thrPct = &vThr
			}

			var accVals, hlthVals, fdVals, thrdVals, estabVals []float64
			for _, x := range s {
				if x.TCP != nil {
					if x.TCP.AcceptQueue != nil {
						accVals = append(accVals, float64(*x.TCP.AcceptQueue))
						if *x.TCP.AcceptQueue >= KneeAcceptQueue {
							acptSamples++
						}
					}
					if x.TCP.States != nil {
						estabVals = append(estabVals, float64(x.TCP.States["ESTAB"]))
					}
				}
				if x.HealthMs != nil {
					hlthVals = append(hlthVals, *x.HealthMs)
				}
				if x.Workers != nil {
					if x.Workers.FDsMax != nil {
						fdVals = append(fdVals, float64(*x.Workers.FDsMax))
					}
					if x.Workers.ThreadsMax != nil {
						thrdVals = append(thrdVals, float64(*x.Workers.ThreadsMax))
					}
				}
			}

			if len(accVals) > 0 {
				vAcc := Pct(accVals, 99)
				acceptq = vAcc
			}
			if len(hlthVals) > 0 {
				healthP99 = Pct(hlthVals, 99)
			}
			if len(fdVals) > 0 {
				fdmax = Pct(fdVals, 99)
			}
			if len(thrdVals) > 0 {
				thrd = Pct(thrdVals, 99)
			}
			if len(estabVals) > 0 {
				estabConnsP99 = Pct(estabVals, 99)
			}
		}

		achieved := float64(len(okReqs)) / dur

		abandoned := 0
		for _, r := range reqs {
			if r.Err != nil && *r.Err == "abandoned" {
				abandoned++
			}
		}

		var accInt, connInt *int
		if acceptq != nil {
			v := int(*acceptq)
			accInt = &v
		}
		if estabConnsP99 != nil {
			v := int(*estabConnsP99)
			connInt = &v
		}

		rows = append(rows, EvalRow{
			RPS:         w.RPS,
			Achieved:    achieved,
			OkCount:     len(okReqs),
			ErrCount:    len(reqs) - len(okReqs),
			Abandoned:   abandoned,
			AcptSamples: acptSamples,
			P50:         Pct(ttfts, 50),
			P95:         Pct(ttfts, 95),
			P99:         Pct(ttfts, 99),
			Cores:       cores,
			CMs:         cMs,
			BaseRAMMB:   baselineRAM,
			AvgRAMMB:    memStats.AvgMB,
			PeakRAMMB:   memStats.PeakMB,
			DeltaRAMMB:  memStats.DeltaMB,
			HealthP99:   healthP99,
			Conns:       connInt,
			AccQueue:    accInt,
			Throttle:    thrPct,
			FDs: func() *int {
				if fdmax == nil {
					return nil
				}
				v := int(*fdmax)
				return &v
			}(),
			Threads: func() *int {
				if thrd == nil {
					return nil
				}
				v := int(*thrd)
				return &v
			}(),
			Ok: true,
		})
	}

	// Pass 2: Tìm mức độ trễ TTFT p50 khỏe tốt nhất (bestP50) và nhận diện hiện tượng Cold-Start ở bậc đầu tiên
	bestP50 := 0.0
	for i := range rows {
		r := &rows[i]
		if r.Achieved >= KneeAchievedRatio*r.RPS && r.Abandoned == 0 && r.P50 != nil && *r.P50 > 0 {
			if bestP50 == 0 || *r.P50 < bestP50 {
				bestP50 = *r.P50
			}
		}
	}

	// Nếu bậc đầu tiên (rows[0]) có TTFT p50 cao gấp > 3 lần một bậc tải cao hơn phía sau (mà bậc sau vẫn đạt throughput & 0 abandoned),
	// thì bậc đầu tiên bị nhiễu bởi Cold-Start (ví dụ giải nén model langid/BM25 ở request đầu tiên).
	if len(rows) >= 2 && rows[0].P50 != nil && *rows[0].P50 >= KneeTTFTMinMs && rows[0].Abandoned == 0 {
		for j := 1; j < len(rows); j++ {
			if rows[j].P50 != nil && *rows[j].P50 > 0 &&
				rows[j].Achieved >= KneeAchievedRatio*rows[j].RPS && rows[j].Abandoned == 0 &&
				*rows[0].P50 >= 3.0*(*rows[j].P50) {
				rows[0].IsColdStart = true
				break
			}
		}
	}

	// Đánh giá Knee và in từng dòng kết quả
	for i := range rows {
		r := &rows[i]
		var reasons []string

		if r.Achieved < KneeAchievedRatio*r.RPS {
			reasons = append(reasons, fmt.Sprintf("achieved<%.0f%%", KneeAchievedRatio*100.0))
		}
		if r.AcptSamples >= KneeAcceptSamples {
			reasons = append(reasons, fmt.Sprintf("acptQ>=%d(%dx)", KneeAcceptQueue, r.AcptSamples))
		}
		if r.HealthP99 != nil && *r.HealthP99 > KneeHealthMs {
			reasons = append(reasons, fmt.Sprintf("hlth>%.0fms", KneeHealthMs))
		}

		// Phát hiện độ trễ TTFT p50 tăng vọt đột biến (>= 4x so với bậc khỏe tốt nhất và >= 1000ms)
		ttftDegraded := false
		if !r.IsColdStart && bestP50 > 0 && r.P50 != nil {
			if *r.P50 >= math.Max(KneeTTFTMinMs, KneeTTFTJumpRatio*bestP50) {
				ttftDegraded = true
				reasons = append(reasons, fmt.Sprintf("ttftP50>=%.0fx", *r.P50/bestP50))
			}
		}

		// Đánh giá CPU Throttling:
		// Trên Pod 1 core, đa luồng burst ngắn thường tạo throttle 10-30% dù CPU trung bình chưa kịch trần và TTFT vẫn rất thấp.
		// Chỉ tính là Knee do throttle khi:
		//   1) Throttle > 50% (nghiêm trọng), HOẶC
		//   2) Throttle > 15% kèm theo CPU đạt >= 85% quota hoặc TTFT đã suy giảm (>= 2.5x bestP50 và >= 500ms).
		if !r.IsColdStart && r.Throttle != nil {
			thr := *r.Throttle
			highCPU := quota != nil && *quota > 0 && r.Cores != nil && *r.Cores >= 0.85*(*quota)
			moderateTTFTJump := bestP50 > 0 && r.P50 != nil && *r.P50 >= math.Max(500.0, 2.5*bestP50)
			if thr > KneeThrottleHardPct || (thr > KneeThrottlePct && (highCPU || ttftDegraded || moderateTTFTJump)) {
				reasons = append(reasons, fmt.Sprintf("thr>%.0f%%", KneeThrottlePct))
			}
		}

		if r.Abandoned > 0 {
			reasons = append(reasons, fmt.Sprintf("abandoned=%d", r.Abandoned))
		}

		verdict := "OK"
		isOk := true
		if len(reasons) > 0 {
			isOk = false
			verdict = "KNEE (" + strings.Join(reasons, ", ") + ")"
		} else if r.IsColdStart {
			verdict = "OK (cold-start)"
		}

		r.Ok = isOk
		r.KneeReason = strings.Join(reasons, ", ")

		var connFloat, accFloat, thrdFloat *float64
		if r.Conns != nil {
			v := float64(*r.Conns)
			connFloat = &v
		}
		if r.AccQueue != nil {
			v := float64(*r.AccQueue)
			accFloat = &v
		}
		if r.Threads != nil {
			v := float64(*r.Threads)
			thrdFloat = &v
		}

		printf("%6s %7s %5d %4d %8s %8s %6s %7s %7s %7s %7s %6s %6s %6s %8s %5s  %s\n",
			FmtNum(&r.RPS, 1),
			FmtNum(&r.Achieved, 1),
			r.OkCount,
			r.ErrCount,
			FmtNum(r.P50, 1),
			FmtNum(r.P95, 1),
			FmtNum(r.Cores, 2),
			FmtNum(r.CMs, 1),
			FmtNum(r.AvgRAMMB, 0),
			FmtNum(r.PeakRAMMB, 0),
			FmtNum(r.DeltaRAMMB, 0),
			FmtNum(r.Throttle, 1),
			FmtNum(connFloat, 0),
			FmtNum(accFloat, 0),
			FmtNum(r.HealthP99, 0),
			FmtNum(thrdFloat, 0),
			verdict,
		)
	}

	println(strings.Repeat("=", 149))
	println("                                           KẾT LUẬN & ĐỊNH CHUẨN")
	println(strings.Repeat("=", 149))

	// 1. Phân tích CPU
	println("▶ 1. ĐỊNH CHUẨN CPU & THROUGHPUT:")
	var healthyC []float64
	var lastOk []EvalRow
	var knees []EvalRow
	anyOk := false

	for _, r := range rows {
		if r.Ok {
			anyOk = true
			lastOk = append(lastOk, r)
			if r.CMs != nil && !r.IsColdStart {
				healthyC = append(healthyC, *r.CMs)
			}
		} else {
			knees = append(knees, r)
		}
	}
	// Fallback nếu chỉ có duy nhất 1 bậc OK và bậc đó dính cold-start
	if len(healthyC) == 0 {
		for _, r := range rows {
			if r.Ok && r.CMs != nil {
				healthyC = append(healthyC, *r.CMs)
			}
		}
	}

	if len(healthyC) > 0 {
		c := Median(healthyC)
		printf("  • C (trung vị các bậc còn khoẻ)    : %.1f ms CPU / request\n", c)
		printf("  • Trần lý thuyết 1 worker (1 core)  : %.1f req/s\n", 1000.0/c)
		if workersSeen > 1 {
			printf("  • Trần lý thuyết %d worker (%d core) : %.1f req/s\n", workersSeen, workersSeen, float64(workersSeen)*1000.0/c)
		}
		if quota != nil && *quota > 0 {
			printf("  • Trần lý thuyết Pod (%g core quota): %.1f req/s\n", *quota, (*quota)*1000.0/c)
			recWorkers := int(math.RoundToEven(*quota))
			if recWorkers < 1 {
				recWorkers = 1
			}
			printf("  • Số worker hợp lý khuyến nghị      : %d worker (= quota)\n", recWorkers)
			if workersSeen > 0 && workersSeen < recWorkers {
				printf("    ⚠️ CẢNH BÁO: Số worker đang chạy (%d) thấp hơn CPU quota (%g core). Cần nâng lên %d worker để khai thác hết quota.\n",
					workersSeen, *quota, recWorkers)
			}
		}
	} else if !anyOk {
		println("  • Chưa có bậc nào 'OK' -> Hãy hạ dải --steps xuống rồi đo lại.")
	} else {
		println("  • Có bậc còn khoẻ nhưng không tính được C -> Thiếu file --probe hoặc probe chưa phủ trùng thời gian.")
	}

	if len(lastOk) > 0 {
		lastOkRPS := lastOk[len(lastOk)-1].RPS
		printf("  • Bậc cao nhất còn đạt chuẩn SLO   : %g req/s\n", lastOkRPS)
		var nextKnees []EvalRow
		for _, r := range rows {
			if !r.Ok && r.RPS > lastOkRPS {
				nextKnees = append(nextKnees, r)
			}
		}
		if len(nextKnees) > 0 {
			printf("  • Điểm gãy quan sát được (KNEE)     : %g req/s (Nguyên nhân: %s)\n", nextKnees[0].RPS, nextKnees[0].KneeReason)
		} else {
			println("  • Chưa chạm điểm gãy sau bậc khoẻ cuối -> Có thể tăng tiếp dải --steps để tìm trần chịu tải.")
		}
	} else if len(knees) > 0 {
		printf("  • Điểm gãy quan sát được            : %g req/s (Nguyên nhân: %s)\n", knees[0].RPS, knees[0].KneeReason)
	}

	// 2. Phân tích RAM
	println("\n▶ 2. ĐỊNH CHUẨN BỘ NHỚ (RAM / MEMORY):")
	memRec := AnalyzeMemory(baselineRAM, rows)
	if memRec != nil {
		printf("  • RAM nền (Baseline khi không tải) : %.1f MB\n", memRec.BaseMB)
		printf("  • RAM đỉnh quan sát được khi tải   : %.1f MB\n", memRec.PeakObservedMB)
		printf("  • Mức tăng RAM tối đa do tải (ΔRAM): %.1f MB\n", math.Max(0.0, memRec.PeakObservedMB-memRec.BaseMB))
		printf("  • Khuyến nghị cấu hình Kubernetes Pod:\n")
		printf("      - resources.requests.memory    : %.0fMi\n", memRec.RecommendedReqMB)
		printf("      - resources.limits.memory      : %.0fMi (bao gồm 30%% buffer chống OOM-Killed)\n", memRec.RecommendedLimMB)
		if memRec.HasGrowthTrend {
			println("    ⚠️ CẢNH BÁO RÒ RỈ RAM: Mức RAM đỉnh liên tục tăng qua các bậc tải và không giảm.")
			println("       Hãy kiểm tra nguy cơ rò rỉ bộ nhớ (connection leak / unbounded cache / buffer tích lũy).")
		}
	} else {
		println("  • Không có dữ liệu đo RAM trong file probe (chưa đọc được memory cgroup).")
	}

	// 3. Phân tích Kết nối & Socket TCP (từ /proc/net/tcp & /proc/<pid>/fd trong probe)
	println("\n▶ 3. ĐỊNH CHUẨN KẾT NỐI & SOCKET TCP (/proc/net/tcp):")
	var (
		hasTCPConns                      bool
		maxEstab, maxAcptQ               int
		maxTimeWait, maxCloseWait, maxFD int
		maxSynRecv, maxSynSent           int
	)
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		if s.TCP != nil {
			if s.TCP.AcceptQueue != nil && *s.TCP.AcceptQueue > maxAcptQ {
				maxAcptQ = *s.TCP.AcceptQueue
			}
			if s.TCP.States != nil {
				hasTCPConns = true
				if v, ok := s.TCP.States["ESTAB"]; ok && v > maxEstab {
					maxEstab = v
				}
				if v, ok := s.TCP.States["TIME_WAIT"]; ok && v > maxTimeWait {
					maxTimeWait = v
				}
				if v, ok := s.TCP.States["CLOSE_WAIT"]; ok && v > maxCloseWait {
					maxCloseWait = v
				}
				if v, ok := s.TCP.States["SYN_RECV"]; ok && v > maxSynRecv {
					maxSynRecv = v
				}
				if v, ok := s.TCP.States["SYN_SENT"]; ok && v > maxSynSent {
					maxSynSent = v
				}
			}
		}
		if s.Workers != nil && s.Workers.FDsMax != nil && *s.Workers.FDsMax > maxFD {
			maxFD = *s.Workers.FDsMax
		}
	}
	if hasTCPConns {
		printf("  • Kết nối TCP ESTABLISHED đỉnh : %d socket (TIME_WAIT: %d, CLOSE_WAIT: %d, SYN_RECV: %d, AcceptQueue: %d)\n",
			maxEstab, maxTimeWait, maxCloseWait, maxSynRecv, maxAcptQ)
		if maxFD > 0 {
			printf("  • File Descriptors tối đa      : %d FDs / worker\n", maxFD)
		}
		if maxCloseWait > 15 {
			printf("    ⚠️ CẢNH BÁO RÒ RỈ SOCKET: Phát hiện CLOSE_WAIT cao (%d). Ứng dụng có nguy cơ quên gọi close() kết nối.\n", maxCloseWait)
		}
		if maxTimeWait > 500 {
			printf("    ⚠️ CẢNH BÁO CHURN KẾT NỐI: TIME_WAIT cao (%d). Kiểm tra lại HTTP Keep-Alive / Connection Pool.\n", maxTimeWait)
		}
		if maxSynRecv > 10 || maxSynSent > 10 {
			printf("    ⚠️ CẢNH BÁO NGHẼN HANDSHAKE: SYN_RECV=%d, SYN_SENT=%d.\n", maxSynRecv, maxSynSent)
		}
	} else {
		println("  • Không có dữ liệu đo kết nối trong file probe.")
	}

	println()

	report := &AnalysisReport{}
	var selectedKneeRow *EvalRow
	if len(lastOk) > 0 {
		report.LastOkRPS = lastOk[len(lastOk)-1].RPS
		var nextKnees []EvalRow
		for _, r := range rows {
			if !r.Ok && r.RPS > report.LastOkRPS {
				nextKnees = append(nextKnees, r)
			}
		}
		if len(nextKnees) > 0 {
			report.HasKnee = true
			report.KneeRPS = nextKnees[0].RPS
			report.KneeReason = nextKnees[0].KneeReason
			selectedKneeRow = &nextKnees[0]
		}
	} else if len(knees) > 0 {
		report.HasKnee = true
		report.KneeRPS = knees[0].RPS
		report.KneeReason = knees[0].KneeReason
		selectedKneeRow = &knees[0]
	}

	if report.HasKnee {
		report.ProfileRPS = report.KneeRPS
		// Nếu bậc Knee đầu tiên đã là bậc sập hoàn toàn (abandoned > 0 hoặc achieved < 50%)
		// và có LastOkRPS phía trước, chọn mức tải trung gian (vùng ứng dụng còn sống và đang chịu áp lực thật)
		if selectedKneeRow != nil && report.LastOkRPS > 0 &&
			(selectedKneeRow.Abandoned > 0 || selectedKneeRow.Achieved < 0.5*selectedKneeRow.RPS) {
			mid := math.Round(((report.LastOkRPS+report.KneeRPS)/2.0)*10.0) / 10.0
			if mid > report.LastOkRPS && mid < report.KneeRPS {
				report.ProfileRPS = mid
			}
		}
	}
	if len(rows) > 0 {
		lastRow := rows[len(rows)-1]
		if lastRow.P95 != nil {
			report.LastP95Ms = *lastRow.P95
		}
	}
	if len(healthyC) > 0 {
		report.TheoreticalC = Median(healthyC)
	}

	return report, nil
}

// SummarizePhase2Probe tổng hợp nhanh dữ liệu probe (CPU, RAM, TCP Socket States từ /proc/net/tcp, FDs)
// thu thập được song song trong suốt quá trình chạy Profiling Pha 2 (py-spy / perf).
func SummarizePhase2Probe(probe []ProbeRecord) string {
	var samples []ProbeRecord
	for _, s := range probe {
		if s.Type != "probe_meta" {
			samples = append(samples, s)
		}
	}
	if len(samples) == 0 {
		return ""
	}

	var buf strings.Builder
	printf := func(format string, a ...interface{}) { fmt.Fprintf(&buf, format, a...) }

	spanS := 0.0
	if len(samples) >= 2 {
		spanS = samples[len(samples)-1].T - samples[0].T
	}

	var cores, thrPct *float64
	if spanS > 0 {
		spanUsec := spanS * 1_000_000.0
		if dUsage := Delta(samples, func(x ProbeRecord) *float64 { return x.UsageUsec }); dUsage != nil {
			v := *dUsage / spanUsec
			cores = &v
		}
		dThr := Delta(samples, func(x ProbeRecord) *float64 { return x.NrThrottled })
		dPer := Delta(samples, func(x ProbeRecord) *float64 { return x.NrPeriods })
		if dThr != nil && dPer != nil && *dPer != 0 {
			v := 100.0 * (*dThr) / (*dPer)
			thrPct = &v
		}
	}

	memStats := StepRAM(samples, nil)

	var (
		estabVals, accVals, hlthVals []float64
		maxEstab, maxTW, maxCW       int
		hasTCP                       bool
	)
	for _, x := range samples {
		if x.TCP != nil {
			if x.TCP.AcceptQueue != nil {
				accVals = append(accVals, float64(*x.TCP.AcceptQueue))
			}
			if x.TCP.States != nil {
				hasTCP = true
				est := x.TCP.States["ESTAB"]
				estabVals = append(estabVals, float64(est))
				if est > maxEstab {
					maxEstab = est
				}
				if v, ok := x.TCP.States["TIME_WAIT"]; ok && v > maxTW {
					maxTW = v
				}
				if v, ok := x.TCP.States["CLOSE_WAIT"]; ok && v > maxCW {
					maxCW = v
				}
			}
		}
		if x.HealthMs != nil {
			hlthVals = append(hlthVals, *x.HealthMs)
		}
	}

	printf("  • Mẫu thu thập Pha 2  : %d mẫu (trong %.0fs profiling)\n", len(samples), spanS)
	if cores != nil || thrPct != nil || memStats.PeakMB != nil {
		printf("  • Tài nguyên Pha 2    : CPU=%s core | Throttle=%s%% | RAM đỉnh=%s MB\n",
			FmtNum(cores, 2), FmtNum(thrPct, 1), FmtNum(memStats.PeakMB, 0))
	}
	if hasTCP {
		estP50 := Pct(estabVals, 50)
		estP99 := Pct(estabVals, 99)
		accP99 := Pct(accVals, 99)
		printf("  • Kết nối TCP (/proc) : ESTAB p50=%s, p99=%s (Đỉnh=%d) | AcceptQ p99=%s | TIME_WAIT=%d | CLOSE_WAIT=%d\n",
			FmtNum(estP50, 0), FmtNum(estP99, 0), maxEstab, FmtNum(accP99, 0), maxTW, maxCW)
	}
	if len(hlthVals) > 0 {
		printf("  • Health /health/live : p99=%s ms\n", FmtNum(Pct(hlthVals, 99), 0))
	}

	return buf.String()
}
