package engine

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"capsizer/pkg/scenarios"
)

// Percentile tính giá trị phân vị p (0-100) từ một tập dữ liệu float64.
func Percentile(xs []float64, p float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	k := int(math.Round((p / 100.0) * float64(len(s)-1)))
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	val := s[k]
	return &val
}

// RunStep thực thi một bậc tải (RPS) duy nhất theo cơ chế Open-Loop.
func RunStep(
	ctx context.Context,
	client *http.Client,
	scenario scenarios.Scenario,
	stepIdx int,
	totalSteps int,
	rps float64,
	stepSeconds float64,
	settleSeconds float64,
	maxInflight int,
	writer RecordWriter,
	statusFilePath string,
	onStatus StatusCallback,
) (StepResult, error) {
	interval := time.Duration(float64(time.Second) / rps)

	tStepStart := UnixTime(time.Now())
	if writer != nil {
		_ = writer.WriteRecord(StepBoundary{Type: "step_start", RPS: rps, T: tStepStart})
		_ = writer.Flush()
	}

	var (
		inflight        atomic.Int64
		maxInflightSeen atomic.Int64
		dropped         atomic.Int64
		n               atomic.Int64
		wg              sync.WaitGroup
	)

	stepDeadline := time.Now().Add(time.Duration(stepSeconds * float64(time.Second)))
	startTime := time.Now()

	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()

	var (
		recMu      sync.Mutex
		records    []scenarios.Record
		collecting = true
	)
	addRecord := func(r scenarios.Record) {
		recMu.Lock()
		defer recMu.Unlock()
		if collecting {
			records = append(records, r)
		}
	}

	var (
		phase           atomic.Int32 // 0: RAMPING, 1: SETTLING
		settleStartNano atomic.Int64
		doneStatus      = make(chan struct{})
	)

	updateStatus := func(now time.Time) {
		curInflight := inflight.Load()
		curDropped := dropped.Load()
		curSent := n.Load() - curDropped
		curPhase := phase.Load()

		var elapsed, remain, elapsedSettle, remainSettle float64
		if curPhase == 0 {
			elapsed = now.Sub(startTime).Seconds()
			if elapsed > stepSeconds {
				elapsed = stepSeconds
			}
			remain = stepSeconds - elapsed
			if remain < 0 {
				remain = 0
			}

			if statusFilePath != "" {
				_ = os.WriteFile(statusFilePath, []byte(fmt.Sprintf(
					"PHASE=RAMPING\nSTEP=%d/%d\nRPS=%.1f\nELAPSED=%.1fs\nDURATION=%.0fs\nREMAIN=%.1fs\nINFLIGHT=%d\nSENT=%d\nDROPPED=%d\nSHOULD_COLLECT=YES\nACTION=DANG_BAN_TAI_THU_THAP_NGAY\n",
					stepIdx, totalSteps, rps, elapsed, stepSeconds, remain, curInflight, curSent, curDropped)), 0644)
			}
		} else {
			sNano := settleStartNano.Load()
			if sNano > 0 {
				elapsedSettle = float64(now.UnixNano()-sNano) / 1e9
			}
			remainSettle = settleSeconds - elapsedSettle
			if remainSettle < 0 {
				remainSettle = 0
			}

			if statusFilePath != "" {
				_ = os.WriteFile(statusFilePath, []byte(fmt.Sprintf(
					"PHASE=SETTLING\nSTEP=%d/%d\nRPS=%.1f\nINFLIGHT=%d\nREMAIN_SETTLE=%.0fs\nSETTLE_TIMEOUT=%.0fs\nSHOULD_COLLECT=YES\nACTION=DANG_DOI_REQUEST_DO_DANG_HOAN_TAT\n",
					stepIdx, totalSteps, rps, curInflight, remainSettle, settleSeconds)), 0644)
			}
		}

		if onStatus != nil {
			onStatus(StatusUpdate{
				Phase:         int(curPhase),
				StepIndex:     stepIdx,
				TotalSteps:    totalSteps,
				RPS:           rps,
				Elapsed:       elapsed,
				Duration:      stepSeconds,
				Remain:        remain,
				Inflight:      curInflight,
				Sent:          curSent,
				Dropped:       curDropped,
				RemainSettle:  remainSettle,
				SettleTimeout: settleSeconds,
			})
		}
	}

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-doneStatus:
				return
			case now := <-ticker.C:
				updateStatus(now)
			}
		}
	}()

launchLoop:
	for {
		select {
		case <-ctx.Done():
			break launchLoop
		default:
		}

		curN := n.Load()
		targetTime := startTime.Add(time.Duration(float64(curN) * float64(interval)))
		now := time.Now()
		if targetTime.After(stepDeadline) || now.After(stepDeadline) {
			break launchLoop
		}

		delay := targetTime.Sub(now)
		if delay > 0 {
			time.Sleep(delay)
		}
		n.Add(1)

		curInflight := inflight.Load()
		if curInflight >= int64(maxInflight) {
			dropped.Add(1)
			continue
		}

		inflight.Add(1)
		for {
			curMax := maxInflightSeen.Load()
			if curInflight+1 <= curMax || maxInflightSeen.CompareAndSwap(curMax, curInflight+1) {
				break
			}
		}

		wg.Add(1)
		go func() {
			defer func() {
				inflight.Add(-1)
				wg.Done()
			}()

			rec := scenario.Execute(stepCtx, client)
			addRecord(rec)
		}()
	}

	// Giai đoạn Settle
	settleStartNano.Store(time.Now().UnixNano())
	phase.Store(1)
	updateStatus(time.Now())

	settleDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(settleDone)
	}()

	select {
	case <-settleDone:
	case <-time.After(time.Duration(settleSeconds * float64(time.Second))):
		cancelStep()
		select {
		case <-settleDone:
		case <-time.After(5 * time.Second):
		}
	}

	close(doneStatus)

	recMu.Lock()
	collecting = false
	launched := n.Load() - dropped.Load()
	abandoned := launched - int64(len(records))

	for i := int64(0); i < abandoned; i++ {
		records = append(records, scenarios.Record{
			Type:   "req",
			TStart: tStepStart,
			Ok:     false,
			Err:    StrPtr("abandoned"),
		})
	}
	recMu.Unlock()

	tStepEnd := UnixTime(time.Now())

	if writer != nil {
		for _, r := range records {
			_ = writer.WriteRecord(r)
		}
		_ = writer.WriteRecord(StepBoundary{Type: "step_end", RPS: rps, T: tStepEnd})
		_ = writer.Flush()
	}

	var okRecords []scenarios.Record
	var metrics []float64
	for _, r := range records {
		if r.Ok {
			okRecords = append(okRecords, r)
			if r.TtftMs != nil {
				metrics = append(metrics, *r.TtftMs)
			}
		}
	}

	completedOk := len(okRecords)
	errorsCount := len(records) - completedOk
	rpsAchieved := float64(completedOk) / math.Max(stepSeconds, 1e-9)

	return StepResult{
		StepIndex:          stepIdx,
		TotalSteps:         totalSteps,
		RpsOffered:         rps,
		Sent:               int(n.Load() - dropped.Load()),
		DroppedByGenerator: int(dropped.Load()),
		CompletedOk:        completedOk,
		Errors:             errorsCount,
		RpsAchieved:        rpsAchieved,
		P50:                Percentile(metrics, 50),
		P95:                Percentile(metrics, 95),
		MaxInflight:        int(maxInflightSeen.Load()),
	}, nil
}
