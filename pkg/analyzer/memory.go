package analyzer

import (
	"math"
)

// MemoryStats chứa các chỉ số RAM đo đạc được của một bước tải.
type MemoryStats struct {
	AvgMB   *float64
	PeakMB  *float64
	DeltaMB *float64
}

// BaselineRAM tính toán mức tiêu thụ RAM trung bình khi không có tải (trước bậc đầu tiên).
func BaselineRAM(probe []ProbeRecord, firstT0 float64) (*float64, int) {
	var pre []float64
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		if s.T < firstT0 && s.MemBytes != nil && *s.MemBytes > 0 {
			pre = append(pre, *s.MemBytes/(1024.0*1024.0))
		}
	}
	if len(pre) == 0 {
		return nil, 0
	}

	sum := 0.0
	for _, v := range pre {
		sum += v
	}
	avg := sum / float64(len(pre))
	return &avg, len(pre)
}

// StepRAM tính toán các chỉ số RAM (trung bình, đỉnh, chênh lệch so với baseline) trong một khoảng thời gian.
func StepRAM(samples []ProbeRecord, baseRAMMB *float64) MemoryStats {
	var mems []float64
	peak := 0.0

	for _, s := range samples {
		if s.MemBytes != nil && *s.MemBytes > 0 {
			mb := *s.MemBytes / (1024.0 * 1024.0)
			mems = append(mems, mb)
			if mb > peak {
				peak = mb
			}
		}
	}

	if len(mems) == 0 {
		return MemoryStats{}
	}

	sum := 0.0
	for _, v := range mems {
		sum += v
	}
	avg := sum / float64(len(mems))

	var delta *float64
	if baseRAMMB != nil {
		d := math.Max(0.0, peak-*baseRAMMB)
		delta = &d
	}

	return MemoryStats{
		AvgMB:   &avg,
		PeakMB:  &peak,
		DeltaMB: delta,
	}
}

// MemoryRecommendation đưa ra gợi ý sizing RAM cho Pod.
type MemoryRecommendation struct {
	BaseMB           float64
	PeakObservedMB   float64
	RecommendedReqMB float64
	RecommendedLimMB float64
	HasGrowthTrend   bool
}

// AnalyzeMemory tổng hợp toàn bộ các bậc và đưa ra khuyến nghị cấu hình Kubernetes Pod.
func AnalyzeMemory(baseRAM *float64, rows []EvalRow) *MemoryRecommendation {
	if len(rows) == 0 {
		return nil
	}

	maxPeak := 0.0
	hasRAMData := false
	var peakSeries []float64

	for _, r := range rows {
		if r.PeakRAMMB != nil {
			hasRAMData = true
			peakSeries = append(peakSeries, *r.PeakRAMMB)
			if *r.PeakRAMMB > maxPeak {
				maxPeak = *r.PeakRAMMB
			}
		}
	}

	if !hasRAMData || maxPeak <= 0 {
		return nil
	}

	baseVal := 0.0
	if baseRAM != nil {
		baseVal = *baseRAM
	} else if len(peakSeries) > 0 {
		baseVal = peakSeries[0]
	}

	// Kiểm tra xu hướng RAM tăng liên tục qua các bậc (dấu hiệu leak / không thu hồi)
	growthCount := 0
	for i := 1; i < len(peakSeries); i++ {
		if peakSeries[i] > peakSeries[i-1]*1.05 { // tăng hơn 5% so với bậc trước
			growthCount++
		}
	}
	hasGrowthTrend := growthCount >= 3

	// Khuyến nghị K8s Sizing:
	// - Request: Đảm bảo đủ cho Peak tải quan sát được
	// - Limit: Thêm 25% - 30% headroom để tránh bị OOM-Killed khi có spike đột ngột
	recReq := math.Ceil(maxPeak*1.1 / 64.0) * 64.0 // Làm tròn lên bội số 64MB
	recLim := math.Ceil(maxPeak*1.3 / 64.0) * 64.0 // Làm tròn lên bội số 64MB

	return &MemoryRecommendation{
		BaseMB:           baseVal,
		PeakObservedMB:   maxPeak,
		RecommendedReqMB: recReq,
		RecommendedLimMB: recLim,
		HasGrowthTrend:   hasGrowthTrend,
	}
}
