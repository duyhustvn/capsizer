package sample

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"capsizer/pkg/scenarios"
)

func init() {
	scenarios.Register("sample-post", func(p scenarios.ScenarioParams) (scenarios.Scenario, error) {
		target := p.TargetURL
		// Nếu URL là endpoint chatbot mặc định hoặc để trống, dùng Public API JSONPlaceholder
		if target == "" || target == "http://127.0.0.1:8000/chat" {
			target = "https://jsonplaceholder.typicode.com/posts"
		}
		bodyData := p.BodyData
		if len(bodyData) == 0 {
			// Payload JSON mặc định gửi đến JSONPlaceholder
			defaultPayload := map[string]any{
				"title":  "Capsizer Load Test",
				"body":   "Benchmarking REST API with open-loop concurrency",
				"userId": 1,
			}
			bodyData, _ = json.Marshal(defaultPayload)
		}
		return NewCreatePostScenario(target, p.Headers, p.Timeout, bodyData), nil
	})
}

// CreatePostScenario kịch bản mẫu gọi API REST POST công khai gửi JSON payload.
type CreatePostScenario struct {
	url      string
	headers  map[string]string
	timeout  time.Duration
	bodyData []byte
}

func NewCreatePostScenario(url string, headers map[string]string, timeout time.Duration, bodyData []byte) *CreatePostScenario {
	hdrs := make(map[string]string)
	for k, v := range headers {
		hdrs[k] = v
	}
	if _, ok := hdrs["Content-Type"]; !ok {
		hdrs["Content-Type"] = "application/json; charset=UTF-8"
	}
	if _, ok := hdrs["Accept"]; !ok {
		hdrs["Accept"] = "application/json"
	}
	return &CreatePostScenario{
		url:      url,
		headers:  hdrs,
		timeout:  timeout,
		bodyData: bodyData,
	}
}

func (s *CreatePostScenario) Name() string        { return "sample-post" }
func (s *CreatePostScenario) MetricLabel() string { return "lat" }

func (s *CreatePostScenario) Setup(ctx context.Context, client *http.Client) error {
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

func (s *CreatePostScenario) Execute(ctx context.Context, client *http.Client) scenarios.Record {
	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	rec := scenarios.Record{
		Type:   "req",
		TStart: tStart,
		Ok:     false,
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var bodyReader io.Reader
	if len(s.bodyData) > 0 {
		bodyReader = bytes.NewReader(s.bodyData)
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.url, bodyReader)
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

	// JSONPlaceholder trả về 201 Created khi tạo post thành công
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		rec.Ok = true
	} else {
		rec.Err = strPtr(fmt.Sprintf("http_%d", resp.StatusCode))
	}
	return rec
}
