package analyzer

import (
	"fmt"
	"math"
	"strings"
)

// RunAnalysis phân tích toàn bộ dữ liệu từ ramp và probe, in bảng báo cáo và kết luận.
func RunAnalysis(ramp []RampRecord, probe []ProbeRecord) error {
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
		return fmt.Errorf("không tìm thấy bậc nào trong file ramp")
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

	fmt.Println(strings.Repeat("=", 142))
	fmt.Println("                             CAPSIZER - KẾT QUẢ ĐỊNH CHUẨN HIỆU NĂNG & NĂNG LỰC")
	fmt.Println(strings.Repeat("=", 142))
	if meta != nil {
		if quota != nil {
			if *quota == math.Trunc(*quota) {
				fmt.Printf("CPU Quota Cores : %.1f core\n", *quota)
			} else {
				fmt.Printf("CPU Quota Cores : %g core\n", *quota)
			}
		} else {
			fmt.Println("CPU Quota Cores : không giới hạn (unlimited)")
		}
		nprocStr := "None"
		if meta.NProcVisible != nil {
			nprocStr = fmt.Sprintf("%d", *meta.NProcVisible)
		}
		fmt.Printf("NProc nhìn thấy : %s (core của Node, không phải quota)\n", nprocStr)
		execStr := "None"
		if meta.DefaultExecutorMaxWorkers != nil {
			execStr = fmt.Sprintf("%d", *meta.DefaultExecutorMaxWorkers)
		}
		fmt.Printf("Executor Threads: %s / worker\n", execStr)
		if workersSeen > 0 {
			fmt.Printf("Workers quan sát: %d worker\n", workersSeen)
		}
	}

	if baselineCPU != nil {
		fmt.Printf("CPU Nền (khi rỗi): %.3f core (%d mẫu / %.0fs)\n", *baselineCPU, baseCPUN, baseCPUSpan)
		if baseCPUSpan < 90 {
			fmt.Printf("  LƯU Ý: Cửa sổ đo nền chỉ %.0fs. Cửa sổ ngắn có thể làm baseline nhảy giữa các lần chạy.\n", baseCPUSpan)
		}
	} else if len(probe) > 0 {
		fmt.Printf("CPU Nền          : KHÔNG đo được (%d mẫu trước bậc đầu, cần >= 3)\n", baseCPUN)
	}

	if baselineRAM != nil {
		fmt.Printf("RAM Nền (khi rỗi): %.1f MB (%d mẫu)\n", *baselineRAM, baseRAMN)
	}

	fmt.Println()

	// In header bảng kết quả (có bổ sung cột RAM)
	cols := fmt.Sprintf("%6s %7s %5s %4s %8s %8s %6s %7s %7s %7s %7s %6s %6s %8s %5s  verdict",
		"offer", "achiev", "ok", "err", "ttftP50", "ttftP95",
		"cores", "C_ms", "ramMB", "pkRAM", "dRAM", "thr%", "acptQ", "hlthP99", "thrd")
	fmt.Println(cols)
	fmt.Println(strings.Repeat("-", 142))

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
			cores, cMs, thrPct                     *float64
			acceptq, healthP99, fdmax, thrd        *float64
			acptSamples                            int
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

			var accVals, hlthVals, fdVals, thrdVals []float64
			for _, x := range s {
				if x.TCP != nil && x.TCP.AcceptQueue != nil {
					accVals = append(accVals, float64(*x.TCP.AcceptQueue))
					if *x.TCP.AcceptQueue >= KneeAcceptQueue {
						acptSamples++
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
		}

		achieved := float64(len(okReqs)) / dur

		// Đánh giá Knee
		var reasons []string
		if achieved < KneeAchievedRatio*w.RPS {
			reasons = append(reasons, fmt.Sprintf("achieved<%.0f%%", KneeAchievedRatio*100.0))
		}
		if acptSamples >= KneeAcceptSamples {
			reasons = append(reasons, fmt.Sprintf("acptQ>=%d(%dx)", KneeAcceptQueue, acptSamples))
		}
		if healthP99 != nil && *healthP99 > KneeHealthMs {
			reasons = append(reasons, fmt.Sprintf("hlth>%.0fms", KneeHealthMs))
		}
		if thrPct != nil && *thrPct > KneeThrottlePct {
			reasons = append(reasons, fmt.Sprintf("thr>%.0f%%", KneeThrottlePct))
		}

		abandoned := 0
		for _, r := range reqs {
			if r.Err != nil && *r.Err == "abandoned" {
				abandoned++
			}
		}
		if abandoned > 0 {
			reasons = append(reasons, fmt.Sprintf("abandoned=%d", abandoned))
		}

		verdict := "OK"
		isOk := true
		if len(reasons) > 0 {
			isOk = false
			verdict = "KNEE (" + strings.Join(reasons, ", ") + ")"
		}

		var accInt *int
		if acceptq != nil {
			v := int(*acceptq)
			accInt = &v
		}

		rows = append(rows, EvalRow{
			RPS:        w.RPS,
			Achieved:   achieved,
			OkCount:    len(okReqs),
			ErrCount:   len(reqs) - len(okReqs),
			P50:        Pct(ttfts, 50),
			P95:        Pct(ttfts, 95),
			P99:        Pct(ttfts, 99),
			Cores:      cores,
			CMs:        cMs,
			BaseRAMMB:  baselineRAM,
			AvgRAMMB:   memStats.AvgMB,
			PeakRAMMB:  memStats.PeakMB,
			DeltaRAMMB: memStats.DeltaMB,
			HealthP99:  healthP99,
			AccQueue:   accInt,
			Throttle:   thrPct,
			FDs:        func() *int { if fdmax == nil { return nil }; v := int(*fdmax); return &v }(),
			Threads:    func() *int { if thrd == nil { return nil }; v := int(*thrd); return &v }(),
			Ok:         isOk,
			KneeReason: strings.Join(reasons, ", "),
		})

		fmt.Printf("%6s %7s %5d %4d %8s %8s %6s %7s %7s %7s %7s %6s %6s %8s %5s  %s\n",
			FmtNum(&w.RPS, 1),
			FmtNum(&achieved, 1),
			len(okReqs),
			len(reqs)-len(okReqs),
			FmtNum(Pct(ttfts, 50), 1),
			FmtNum(Pct(ttfts, 95), 1),
			FmtNum(cores, 2),
			FmtNum(cMs, 1),
			FmtNum(memStats.AvgMB, 0),
			FmtNum(memStats.PeakMB, 0),
			FmtNum(memStats.DeltaMB, 0),
			FmtNum(thrPct, 1),
			FmtNum(acceptq, 0),
			FmtNum(healthP99, 0),
			FmtNum(thrd, 0),
			verdict,
		)
	}

	fmt.Println(strings.Repeat("=", 142))
	fmt.Println("                                           KẾT LUẬN & ĐỊNH CHUẨN")
	fmt.Println(strings.Repeat("=", 142))

	// 1. Phân tích CPU
	fmt.Println("▶ 1. ĐỊNH CHUẨN CPU & THROUGHPUT:")
	var healthyC []float64
	var lastOk []EvalRow
	var knees []EvalRow
	anyOk := false

	for _, r := range rows {
		if r.Ok {
			anyOk = true
			lastOk = append(lastOk, r)
			if r.CMs != nil {
				healthyC = append(healthyC, *r.CMs)
			}
		} else {
			knees = append(knees, r)
		}
	}

	if len(healthyC) > 0 {
		c := Median(healthyC)
		fmt.Printf("  • C (trung vị các bậc còn khoẻ)    : %.1f ms CPU / request\n", c)
		fmt.Printf("  • Trần lý thuyết 1 worker (1 core)  : %.1f req/s\n", 1000.0/c)
		if workersSeen > 1 {
			fmt.Printf("  • Trần lý thuyết %d worker (%d core) : %.1f req/s\n", workersSeen, workersSeen, float64(workersSeen)*1000.0/c)
		}
		if quota != nil && *quota > 0 {
			fmt.Printf("  • Trần lý thuyết Pod (%g core quota): %.1f req/s\n", *quota, (*quota)*1000.0/c)
			recWorkers := int(math.RoundToEven(*quota))
			if recWorkers < 1 {
				recWorkers = 1
			}
			fmt.Printf("  • Số worker hợp lý khuyến nghị      : %d worker (= quota)\n", recWorkers)
			if workersSeen > 0 && workersSeen < recWorkers {
				fmt.Printf("    ⚠️ CẢNH BÁO: Số worker đang chạy (%d) thấp hơn CPU quota (%g core). Cần nâng lên %d worker để khai thác hết quota.\n",
					workersSeen, *quota, recWorkers)
			}
		}
	} else if !anyOk {
		fmt.Println("  • Chưa có bậc nào 'OK' -> Hãy hạ dải --steps xuống rồi đo lại.")
	} else {
		fmt.Println("  • Có bậc còn khoẻ nhưng không tính được C -> Thiếu file --probe hoặc probe chưa phủ trùng thời gian.")
	}

	if len(lastOk) > 0 {
		lastOkRPS := lastOk[len(lastOk)-1].RPS
		fmt.Printf("  • Bậc cao nhất còn đạt chuẩn SLO   : %g req/s\n", lastOkRPS)
		var nextKnees []EvalRow
		for _, r := range rows {
			if !r.Ok && r.RPS > lastOkRPS {
				nextKnees = append(nextKnees, r)
			}
		}
		if len(nextKnees) > 0 {
			fmt.Printf("  • Điểm gãy quan sát được (KNEE)     : %g req/s (Nguyên nhân: %s)\n", nextKnees[0].RPS, nextKnees[0].KneeReason)
		} else {
			fmt.Println("  • Chưa chạm điểm gãy sau bậc khoẻ cuối -> Có thể tăng tiếp dải --steps để tìm trần chịu tải.")
		}
	} else if len(knees) > 0 {
		fmt.Printf("  • Điểm gãy quan sát được            : %g req/s (Nguyên nhân: %s)\n", knees[0].RPS, knees[0].KneeReason)
	}

	// 2. Phân tích RAM (BỔ SUNG MỚI)
	fmt.Println("\n▶ 2. ĐỊNH CHUẨN BỘ NHỚ (RAM / MEMORY):")
	memRec := AnalyzeMemory(baselineRAM, rows)
	if memRec != nil {
		fmt.Printf("  • RAM nền (Baseline khi không tải) : %.1f MB\n", memRec.BaseMB)
		fmt.Printf("  • RAM đỉnh quan sát được khi tải   : %.1f MB\n", memRec.PeakObservedMB)
		fmt.Printf("  • Mức tăng RAM tối đa do tải (ΔRAM): %.1f MB\n", math.Max(0.0, memRec.PeakObservedMB-memRec.BaseMB))
		fmt.Printf("  • Khuyến nghị cấu hình Kubernetes Pod:\n")
		fmt.Printf("      - resources.requests.memory    : %.0fMi\n", memRec.RecommendedReqMB)
		fmt.Printf("      - resources.limits.memory      : %.0fMi (bao gồm 30%% buffer chống OOM-Killed)\n", memRec.RecommendedLimMB)
		if memRec.HasGrowthTrend {
			fmt.Println("    ⚠️ CẢNH BÁO RÒ RỈ RAM: Mức RAM đỉnh liên tục tăng qua các bậc tải và không giảm.")
			fmt.Println("       Hãy kiểm tra nguy cơ rò rỉ bộ nhớ (connection leak / unbounded cache / buffer tích lũy).")
		}
	} else {
		fmt.Println("  • Không có dữ liệu đo RAM trong file probe (chưa đọc được memory cgroup).")
	}

	fmt.Println()
	return nil
}
