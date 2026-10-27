package engine

import (
	"context"
	"fmt"
	"time"
)

// Engine quản lý toàn bộ quá trình phát tải qua các bậc RPS.
type Engine struct {
	cfg    EngineConfig
	writer RecordWriter
}

// NewEngine khởi tạo engine mới với cấu hình và record writer.
func NewEngine(cfg EngineConfig, writer RecordWriter) *Engine {
	return &Engine{
		cfg:    cfg,
		writer: writer,
	}
}

// Run thực thi toàn bộ kịch bản kiểm thử tải qua các bậc.
func (e *Engine) Run(ctx context.Context) ([]StepResult, error) {
	// 1. Ghi RunMeta ban đầu
	if e.writer != nil {
		meta := RunMeta{
			Type:        "run_meta",
			Scenario:    e.cfg.Scenario.Name(),
			Steps:       e.cfg.Steps,
			StepSeconds: e.cfg.StepSeconds,
		}
		_ = e.writer.WriteRecord(meta)
		_ = e.writer.Flush()
	}

	// 2. Chạy Setup kịch bản (nếu có chuẩn bị dữ liệu / token)
	if err := e.cfg.Scenario.Setup(ctx, e.cfg.HTTPClient); err != nil {
		return nil, fmt.Errorf("lỗi khởi tạo kịch bản: %w", err)
	}

	// 3. Baseline wait nếu được yêu cầu
	if e.cfg.BaselineWait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(e.cfg.BaselineWait * float64(time.Second))):
		}
	}

	var results []StepResult
	totalSteps := len(e.cfg.Steps)

	// 4. Lặp qua từng bậc RPS
	for idx, rps := range e.cfg.Steps {
		stepIdx := idx + 1
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		res, err := RunStep(
			ctx,
			e.cfg.HTTPClient,
			e.cfg.Scenario,
			stepIdx,
			totalSteps,
			rps,
			e.cfg.StepSeconds,
			e.cfg.SettleSeconds,
			e.cfg.MaxInflight,
			e.writer,
			e.cfg.StatusFilePath,
			e.cfg.OnStatus,
		)
		if err != nil {
			return results, err
		}

		results = append(results, res)
		if e.cfg.OnStepComplete != nil {
			e.cfg.OnStepComplete(res)
		}

		// Cooldown giữa 2 bậc (trừ bậc cuối)
		if stepIdx < totalSteps && e.cfg.Cooldown > 0 {
			select {
			case <-ctx.Done():
				return results, ctx.Err()
			case <-time.After(time.Duration(e.cfg.Cooldown * float64(time.Second))):
			}
		}
	}

	return results, nil
}
