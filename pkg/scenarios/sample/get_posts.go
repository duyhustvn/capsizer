package sample

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"capsizer/pkg/scenarios"
)

func init() {
	scenarios.Register("sample-get", func(p scenarios.ScenarioParams) (scenarios.Scenario, error) {
		target := p.TargetURL
		// Nếu URL là endpoint chatbot mặc định hoặc để trống, dùng Public API JSONPlaceholder
		if target == "" || target == "http://127.0.0.1:8000/chat" {
			target = "https://jsonplaceholder.typicode.com/posts/1"
		}
		return NewGetPostsScenario(target, p.Headers, p.Timeout), nil
	})
}

// GetPostsScenario kịch bản mẫu gọi API REST GET công khai (JSONPlaceholder).
type GetPostsScenario struct {
	url     string
	headers map[string]string
	timeout time.Duration
}

func NewGetPostsScenario(url string, headers map[string]string, timeout time.Duration) *GetPostsScenario {
	hdrs := make(map[string]string)
	for k, v := range headers {
		hdrs[k] = v
	}
	if _, ok := hdrs["Accept"]; !ok {
		hdrs["Accept"] = "application/json"
	}
	return &GetPostsScenario{
		url:     url,
		headers: hdrs,
		timeout: timeout,
	}
}

func (s *GetPostsScenario) Name() string        { return "sample-get" }
func (s *GetPostsScenario) MetricLabel() string { return "lat" }

func (s *GetPostsScenario) Setup(ctx context.Context, client *http.Client) error {
	t0 := time.Now()
	setupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rec := s.Execute(setupCtx, client)
	elapsed := time.Since(t0)

	if !rec.Ok {
		errDetail := "unknown error"
		if rec.Err != nil {
			errDetail = *rec.Err
		}
		statusStr := "không có HTTP response"
		if rec.Status != nil {
			statusStr = fmt.Sprintf("HTTP %d", *rec.Status)
		}
		return fmt.Errorf("warmup thất bại (%s): %s", statusStr, errDetail)
	}

	statusCode := 0
	if rec.Status != nil {
		statusCode = *rec.Status
	}
	fmt.Printf("warmup   : kiểm tra kết nối Public API (%s)... OK (status: %d, latency: %v)\n", s.url, statusCode, elapsed.Round(time.Millisecond))
	return nil
}

func (s *GetPostsScenario) Execute(ctx context.Context, client *http.Client) scenarios.Record {
	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	rec := scenarios.Record{
		Type:   "req",
		TStart: tStart,
		Ok:     false,
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.url, nil)
	if err != nil {
		rec.Err = strPtr(err.Error())
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		rec.TtftMs = &elapsed
		return rec
	}

	for k, v := range s.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			rec.Err = strPtr("abandoned")
		} else {
			rec.Err = strPtr(err.Error())
		}
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		rec.TtftMs = &elapsed
		return rec
	}
	defer resp.Body.Close()

	rec.Status = &resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)

	elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
	rec.TotalMs = &elapsed
	rec.TtftMs = &elapsed

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		rec.Ok = true
	} else {
		rec.Err = strPtr(fmt.Sprintf("http_%d", resp.StatusCode))
	}
	return rec
}

func strPtr(s string) *string {
	return &s
}
