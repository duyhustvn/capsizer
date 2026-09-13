package scenarios

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RestScenario kịch bản kiểm thử API REST thông thường (GET, POST, PUT, DELETE,...).
type RestScenario struct {
	url      string
	method   string
	headers  map[string]string
	timeout  time.Duration
	bodyData []byte
}

func NewRestScenario(
	url string,
	method string,
	headers map[string]string,
	timeout time.Duration,
	bodyData []byte,
) *RestScenario {
	hdrs := make(map[string]string)
	for k, v := range headers {
		hdrs[k] = v
	}
	m := strings.ToUpper(method)
	if m == "" {
		m = "GET"
	}
	return &RestScenario{
		url:      url,
		method:   m,
		headers:  hdrs,
		timeout:  timeout,
		bodyData: bodyData,
	}
}

func (s *RestScenario) Name() string        { return "rest" }
func (s *RestScenario) MetricLabel() string { return "lat" }
func (s *RestScenario) Setup(ctx context.Context, client *http.Client) error {
	return nil
}

func (s *RestScenario) Execute(ctx context.Context, client *http.Client) Record {
	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	rec := Record{
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

	httpReq, err := http.NewRequestWithContext(reqCtx, s.method, s.url, bodyReader)
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
		rec.Err = strPtr(err.Error())
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
