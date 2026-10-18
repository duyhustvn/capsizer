package chatbot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"ramps/golang/scenarios"
)

func init() {
	scenarios.Register("chat-sse", func(p scenarios.ScenarioParams) (scenarios.Scenario, error) {
		if len(p.Queries) == 0 {
			return nil, errors.New("không tìm thấy danh sách câu hỏi kiểm thử. Vui lòng cung cấp file câu hỏi qua cờ --queries hoặc khai báo CHAT_QUERIES / CHAT_QUERIES_FILE trong .env")
		}
		pool := p.UserPool
		if pool == nil {
			pool = scenarios.NewUserPool([]scenarios.User{{Token: "loadtest-token"}})
		}
		return NewChatSSEScenario(p.TargetURL, p.Headers, p.Timeout, pool, p.Queries, p.CacheBust), nil
	})
}

// ChatSSEScenario kịch bản kiểm thử API Chatbot streaming SSE.
type ChatSSEScenario struct {
	url       string
	headers   map[string]string
	timeout   time.Duration
	pool      *scenarios.UserPool
	queries   []string
	cacheBust bool
}

func NewChatSSEScenario(
	url string,
	headers map[string]string,
	timeout time.Duration,
	pool *scenarios.UserPool,
	queries []string,
	cacheBust bool,
) *ChatSSEScenario {
	hdrs := make(map[string]string)
	for k, v := range headers {
		hdrs[k] = v
	}
	if _, ok := hdrs["Content-Type"]; !ok {
		hdrs["Content-Type"] = "application/json"
	}
	return &ChatSSEScenario{
		url:       url,
		headers:   hdrs,
		timeout:   timeout,
		pool:      pool,
		queries:   queries,
		cacheBust: cacheBust,
	}
}

func (s *ChatSSEScenario) Name() string        { return "chat-sse" }
func (s *ChatSSEScenario) MetricLabel() string { return "ttft" }
func (s *ChatSSEScenario) Setup(ctx context.Context, client *http.Client) error {
	// Thực hiện 1 request smoke-test / dry-run:
	// 1. Kiểm tra kết nối, URL, Authentication (fail-fast)
	// 2. Làm ấm connection pool (TCP/TLS handshake) của http.Client
	// 3. Đánh thức backend/worker lần đầu
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

	ttftStr := "-"
	if rec.TtftMs != nil {
		ttftStr = fmt.Sprintf("%.0fms", *rec.TtftMs)
	}
	fmt.Printf("warmup   : kiểm tra kết nối & làm ấm connection pool... OK (TTFT: %s, total: %v)\n", ttftStr, elapsed.Round(time.Millisecond))
	return nil
}

func (s *ChatSSEScenario) Execute(ctx context.Context, client *http.Client) scenarios.Record {
	q := "Xin chào"
	if len(s.queries) > 0 {
		q = s.queries[rand.Intn(len(s.queries))]
	}
	if s.cacheBust {
		q = fmt.Sprintf("%s (ma tham chieu %d)", q, rand.Intn(900000)+100000)
	}

	userID, token := s.pool.Next()
	bodyBytes, _ := json.Marshal(map[string]any{
		"message": q,
		"stream":  true,
		"model":   "default",
	})

	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	endSeen := false
	rec := scenarios.Record{
		Type:     "req",
		TStart:   tStart,
		UserID:   userID,
		TtftMs:   nil,
		NoticeMs: nil,
		TotalMs:  nil,
		Status:   nil,
		Ok:       false,
		Err:      nil,
		EndSeen:  &endSeen,
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", s.url, bytes.NewReader(bodyBytes))
	if err != nil {
		rec.Err = strPtr(err.Error())
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		return rec
	}

	for k, v := range s.headers {
		httpReq.Header.Set(k, v)
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
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
		return rec
	}
	defer resp.Body.Close()

	rec.Status = &resp.StatusCode
	if resp.StatusCode != 200 {
		rec.Err = strPtr(fmt.Sprintf("http_%d", resp.StatusCode))
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		_, _ = io.Copy(io.Discard, resp.Body)
		return rec
	}

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var event string

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "event:") {
				event = strings.TrimSpace(trimmed[6:])
			} else if strings.HasPrefix(trimmed, "data:") {
				now := float64(time.Since(t0).Microseconds()) / 1000.0
				if event == "message" && rec.TtftMs == nil {
					rec.TtftMs = &now
				} else if event == "notice" && rec.NoticeMs == nil {
					rec.NoticeMs = &now
				} else if event == "message_end" {
					*rec.EndSeen = true
				} else if event == "error" {
					rec.Err = strPtr("sse_error")
				}
			}
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				if rec.Err == nil {
					rec.Err = strPtr("abandoned")
				}
			} else if err != io.EOF {
				if rec.Err == nil {
					rec.Err = strPtr(err.Error())
				}
			}
			break
		}
	}

	elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
	rec.TotalMs = &elapsed
	rec.Ok = *rec.EndSeen && rec.Err == nil
	return rec
}

func strPtr(s string) *string {
	return &s
}
