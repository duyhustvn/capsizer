package analyzer

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// LoadRamp đọc và phân tích file ramp.jsonl.
func LoadRamp(path string) ([]RampRecord, error) {
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

// LoadProbe đọc và phân tích file probe.jsonl.
func LoadProbe(path string) ([]ProbeRecord, error) {
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

// Pct tính phân vị thứ p theo phương pháp nearest-rank.
func Pct(xs []float64, p float64) *float64 {
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

// Median tính trung vị của danh sách số thực.
func Median(xs []float64) float64 {
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

// FmtNum định dạng số thực hiển thị trên bảng kết quả.
func FmtNum(v *float64, nd int) string {
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

// BuildWindows phân tách các bản ghi từ ramp.jsonl thành các khung thời gian tương ứng với từng mức RPS.
func BuildWindows(ramp []RampRecord, stepSeconds *float64) []StepWindow {
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

// ProbeSlice lọc các mẫu dữ liệu probe rơi vào khoảng thời gian [t0, t1].
func ProbeSlice(probe []ProbeRecord, t0, t1 float64) []ProbeRecord {
	var out []ProbeRecord
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		t := s.T
		if t0 <= t && t <= t1 {
			out = append(out, s)
		}
	}
	return out
}

// Delta tính độ chênh lệch (cuối trừ đầu) của giá trị tích lũy.
func Delta(samples []ProbeRecord, getter func(ProbeRecord) *float64) *float64 {
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

// BaselineCPU tính mức tiêu thụ CPU nền khi chưa có tải.
func BaselineCPU(probe []ProbeRecord, firstT0 float64) (*float64, int, float64) {
	var pre []ProbeRecord
	for _, s := range probe {
		if s.Type == "probe_meta" {
			continue
		}
		if s.T < firstT0 {
			pre = append(pre, s)
		}
	}
	if len(pre) < 3 {
		return nil, len(pre), 0.0
	}
	spanS := pre[len(pre)-1].T - pre[0].T
	d := Delta(pre, func(s ProbeRecord) *float64 { return s.UsageUsec })
	if d == nil || *d == 0 || spanS <= 0 {
		return nil, len(pre), math.Max(0.0, spanS)
	}
	val := *d / (spanS * 1_000_000.0)
	return &val, len(pre), spanS
}
