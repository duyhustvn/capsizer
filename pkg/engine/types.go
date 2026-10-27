package engine

import (
	"net/http"
	"time"

	"capsizer/pkg/scenarios"
)

// RunMeta chứa thông tin tổng thể về phiên kiểm thử tải.
type RunMeta struct {
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	Scenario    string    `json:"scenario"`
	Steps       []float64 `json:"steps"`
	StepSeconds float64   `json:"step_seconds"`
}

// StepBoundary đại diện cho mốc bắt đầu hoặc kết thúc một bậc RPS trong file jsonl.
type StepBoundary struct {
	Type string  `json:"type"`
	RPS  float64 `json:"rps"`
	T    float64 `json:"t"`
}

// StepResult tóm tắt kết quả đo đạc sau khi hoàn tất một bậc RPS.
type StepResult struct {
	StepIndex          int
	TotalSteps         int
	RpsOffered         float64
	Sent               int
	DroppedByGenerator int
	CompletedOk        int
	Errors             int
	RpsAchieved        float64
	P50                *float64
	P95                *float64
	MaxInflight        int
}

// StatusUpdate chứa thông tin tiến độ thời gian thực của step đang chạy.
type StatusUpdate struct {
	Phase         int     // 0: RAMPING, 1: SETTLING
	StepIndex     int
	TotalSteps    int
	RPS           float64
	Elapsed       float64
	Duration      float64
	Remain        float64
	Inflight      int64
	Sent          int64
	Dropped       int64
	RemainSettle  float64
	SettleTimeout float64
}

// StatusCallback là hàm nhận thông tin tiến độ thời gian thực để hiển thị trên UI.
type StatusCallback func(status StatusUpdate)

// StepCallback là hàm gọi lại sau khi một bậc RPS hoàn thành.
type StepCallback func(result StepResult)

// RecordWriter là interface để ghi các bản ghi ra file jsonl hoặc buffer.
type RecordWriter interface {
	WriteRecord(record any) error
	Flush() error
}

// EngineConfig chứa toàn bộ tham số cấu hình bộ phát tải.
type EngineConfig struct {
	Scenario       scenarios.Scenario
	HTTPClient     *http.Client
	Steps          []float64
	StepSeconds    float64
	Cooldown       float64
	SettleSeconds  float64
	BaselineWait   float64
	MaxInflight    int
	StatusFilePath string
	OnStatus       StatusCallback
	OnStepComplete StepCallback
}

// Helper đổi time.Time sang Unix timestamp dạng giây (float64).
func UnixTime(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// Helper lấy con trỏ string.
func StrPtr(s string) *string {
	return &s
}
