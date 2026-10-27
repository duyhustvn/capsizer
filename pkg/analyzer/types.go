package analyzer

// Các ngưỡng nhận diện điểm nghẽn hệ thống (saturation point).
const (
	KneeAchievedRatio = 0.90   // Throughput thực tế đạt được < 90% mức yêu cầu
	KneeAcceptQueue   = 1      // Hàng đợi TCP socket lắng nghe có kết nối chờ xử lý
	KneeAcceptSamples = 3      // Số mẫu tối thiểu thấy accept queue > 0 (chống false knee 1 mẫu)
	KneeHealthMs      = 1000.0 // Độ trễ /health/live vượt quá 1000ms (event loop tắc nghẽn)
	KneeThrottlePct   = 5.0    // Tỷ lệ chu kỳ CPU bị bóp nghẽn CFS quota > 5%
)

// RampRecord đại diện cho một bản ghi trong file loadtest-ramp.jsonl.
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

// ProbeTCP chứa trạng thái kết nối TCP và hàng đợi lắng nghe.
type ProbeTCP struct {
	AcceptQueue *int           `json:"accept_queue"`
	BacklogMax  *int           `json:"backlog_max"`
	States      map[string]int `json:"states"`
}

// ProbeWorkers chứa thông tin tài nguyên tiến trình worker.
type ProbeWorkers struct {
	NProcs     *int `json:"n_procs"`
	NWorkers   *int `json:"n_workers"`
	FDsTotal   *int `json:"fds_total"`
	FDsMax     *int `json:"fds_max"`
	ThreadsMax *int `json:"threads_max"`
}

// ProbeRecord đại diện cho một bản ghi trong file probe.jsonl.
type ProbeRecord struct {
	Type                      string        `json:"type"`
	T                         float64       `json:"t"`
	CPUQuotaCores             *float64      `json:"cpu_quota_cores"`
	NProcVisible              *int          `json:"nproc_visible"`
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

// StepWindow lưu thông tin phạm vi thời gian một bậc RPS.
type StepWindow struct {
	RPS     float64
	T0      float64
	T1      float64
	LaunchS float64
	Reqs    []RampRecord
}

// EvalRow lưu kết quả tính toán chi tiết của từng bậc kiểm thử.
type EvalRow struct {
	RPS        float64
	Achieved   float64
	OkCount    int
	ErrCount   int
	P50        *float64
	P95        *float64
	P99        *float64
	Cores      *float64
	CMs        *float64
	BaseRAMMB  *float64
	AvgRAMMB   *float64
	PeakRAMMB  *float64
	DeltaRAMMB *float64
	HealthP99  *float64
	AccQueue   *int
	Throttle   *float64
	Threads    *int
	FDs        *int
	Ok         bool
	KneeReason string
}
