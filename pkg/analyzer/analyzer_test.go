package analyzer

import (
	"strings"
	"testing"
)

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }
func strPtr(s string) *string     { return &s }

func TestAnalyze_Run6ColdStartAndSteadyStateKnee(t *testing.T) {
	stepSec := 10.0
	ramp := []RampRecord{
		{Type: "run_meta", StepSeconds: floatPtr(stepSec)},
	}

	addStepReqs := func(rps, t0, ttftMs float64, okCount, abandonedCount int) {
		ramp = append(ramp, RampRecord{Type: "step_start", RPS: rps, T: t0})
		total := okCount + abandonedCount
		for i := 0; i < total; i++ {
			tStart := t0 + float64(i)*(stepSec/float64(total))
			rec := RampRecord{
				Type:   "req",
				RPS:    rps,
				TStart: floatPtr(tStart),
				T:      tStart + ttftMs/1000.0,
			}
			if i < okCount {
				rec.Ok = true
				rec.TtftMs = floatPtr(ttftMs)
			} else {
				rec.Ok = false
				rec.Err = strPtr("abandoned")
			}
			ramp = append(ramp, rec)
		}
		ramp = append(ramp, RampRecord{Type: "step_end", RPS: rps, T: t0 + stepSec})
	}

	// Mô phỏng chính xác dữ liệu Run 6 trong ảnh:
	// - Bậc 2 rps (t=10..20): TTFT p50 = 27500ms (cold-start langid/bz2), throttle = 12.1%, 0 lỗi
	// - Bậc 4 rps (t=30..40): TTFT p50 = 225ms (healthy steady-state), throttle = 24.6%, 0 lỗi
	// - Bậc 8 rps (t=50..60): TTFT p50 = 4000ms (nhảy 18x -> Knee thật), throttle = 16.4%, 0 lỗi
	// - Bậc 10 rps (t=70..80): TTFT p50 = 3900ms, abandoned = 50 (app sập)
	addStepReqs(2.0, 10.0, 27500.0, 20, 0)
	addStepReqs(4.0, 30.0, 225.0, 40, 0)
	addStepReqs(8.0, 50.0, 4000.0, 80, 0)
	addStepReqs(10.0, 70.0, 3900.0, 50, 50)

	probe := []ProbeRecord{
		{Type: "probe_meta", T: 0.0, CPUQuotaCores: floatPtr(1.0)},
		// Baseline (t=1..5)
		{T: 1.0, UsageUsec: floatPtr(0), NrPeriods: floatPtr(0), NrThrottled: floatPtr(0)},
		{T: 5.0, UsageUsec: floatPtr(40_000), NrPeriods: floatPtr(40), NrThrottled: floatPtr(0)},
		// Step 2 rps (t=10..20): throttle 12.1%, cores ~0.60
		{T: 10.0, UsageUsec: floatPtr(100_000), NrPeriods: floatPtr(100), NrThrottled: floatPtr(0)},
		{T: 20.0, UsageUsec: floatPtr(6_100_000), NrPeriods: floatPtr(200), NrThrottled: floatPtr(12)},
		// Step 4 rps (t=30..40): throttle 24.6%, cores ~0.55 (chưa kịch quota 1 core, TTFT 225ms rất mượt)
		{T: 30.0, UsageUsec: floatPtr(7_000_000), NrPeriods: floatPtr(300), NrThrottled: floatPtr(12)},
		{T: 40.0, UsageUsec: floatPtr(12_500_000), NrPeriods: floatPtr(400), NrThrottled: floatPtr(37)},
		// Step 8 rps (t=50..60): throttle 16.4%, cores ~0.92, TTFT vọt lên 4000ms (18x)
		{T: 50.0, UsageUsec: floatPtr(13_000_000), NrPeriods: floatPtr(500), NrThrottled: floatPtr(37)},
		{T: 60.0, UsageUsec: floatPtr(22_200_000), NrPeriods: floatPtr(600), NrThrottled: floatPtr(53)},
		// Step 10 rps (t=70..80): throttle 23.1%, cores ~0.95
		{T: 70.0, UsageUsec: floatPtr(23_000_000), NrPeriods: floatPtr(700), NrThrottled: floatPtr(53)},
		{T: 80.0, UsageUsec: floatPtr(32_500_000), NrPeriods: floatPtr(800), NrThrottled: floatPtr(76)},
	}

	report, err := Analyze(ramp, probe)
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}

	if report.LastOkRPS != 4.0 {
		t.Errorf("LastOkRPS = %.1f; want 4.0 (report:\n%s)", report.LastOkRPS, report.ReportText)
	}
	if !report.HasKnee || report.KneeRPS != 8.0 {
		t.Errorf("KneeRPS = %.1f (HasKnee=%v); want 8.0 (report:\n%s)", report.KneeRPS, report.HasKnee, report.ReportText)
	}
	if report.ProfileRPS != 8.0 {
		t.Errorf("ProfileRPS = %.1f; want 8.0", report.ProfileRPS)
	}
}

func TestAnalyze_ConnectionsAndPhase2Summary(t *testing.T) {
	stepSec := 10.0
	ramp := []RampRecord{
		{Type: "run_meta", StepSeconds: floatPtr(stepSec)},
		{Type: "step_start", RPS: 4.0, T: 10.0},
		{Type: "req", RPS: 4.0, TStart: floatPtr(11.0), T: 11.2, Ok: true, TtftMs: floatPtr(200.0)},
		{Type: "step_end", RPS: 4.0, T: 20.0},
	}

	probe := []ProbeRecord{
		{Type: "probe_meta", T: 0.0, CPUQuotaCores: floatPtr(1.0)},
		{
			T:           12.0,
			UsageUsec:   floatPtr(1_000_000),
			NrPeriods:   floatPtr(100),
			NrThrottled: floatPtr(5),
			TCP: &ProbeTCP{
				AcceptQueue: intPtr(0),
				States:      map[string]int{"ESTAB": 18, "TIME_WAIT": 10, "CLOSE_WAIT": 2},
			},
		},
		{
			T:           18.0,
			UsageUsec:   floatPtr(4_000_000),
			NrPeriods:   floatPtr(160),
			NrThrottled: floatPtr(8),
			TCP: &ProbeTCP{
				AcceptQueue: intPtr(0),
				States:      map[string]int{"ESTAB": 20, "TIME_WAIT": 12, "CLOSE_WAIT": 2},
			},
		},
	}

	report, err := Analyze(ramp, probe)
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}
	if !strings.Contains(report.ReportText, "conns") {
		t.Errorf("expected report header to include 'conns', got:\n%s", report.ReportText)
	}
	if !strings.Contains(report.ReportText, "ĐỊNH CHUẨN KẾT NỐI") || !strings.Contains(report.ReportText, "20 socket") {
		t.Errorf("expected report to include section 3 with 20 ESTAB sockets, got:\n%s", report.ReportText)
	}

	p2Summary := SummarizePhase2Probe(probe)
	if !strings.Contains(p2Summary, "Đỉnh=20") {
		t.Errorf("expected SummarizePhase2Probe to include peak 20 ESTAB sockets, got:\n%s", p2Summary)
	}
}
