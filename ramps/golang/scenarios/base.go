package scenarios

import (
	"context"
	"net/http"
)

// Record đại diện cho một bản ghi kết quả request chuẩn hóa để xuất ra JSONL.
// report.py yêu cầu cấu trúc này để tính toán các phân vị và điểm gãy.
type Record struct {
	Type     string   `json:"type"`
	TStart   float64  `json:"t_start,omitempty"`
	UserID   string   `json:"user_id,omitempty"`
	TtftMs   *float64 `json:"ttft_ms"`
	NoticeMs *float64 `json:"notice_ms,omitempty"`
	TotalMs  *float64 `json:"total_ms"`
	Status   *int     `json:"status"`
	Ok       bool     `json:"ok"`
	Err      *string  `json:"err"`
	EndSeen  *bool    `json:"end_seen,omitempty"`
}

// Scenario là giao diện cơ sở (Interface/Protocol) cho mọi kịch bản kiểm thử API.
// Tương đương với class Scenario trong scenarios/base.py.
type Scenario interface {
	Name() string
	MetricLabel() string
	Setup(ctx context.Context, client *http.Client) error
	Execute(ctx context.Context, client *http.Client) Record
}
