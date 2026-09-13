package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var DefaultQueries = []string{
	"Xin chào, bạn có thể hỗ trợ những tác vụ gì?",
	"Hãy tóm tắt ngắn gọn các điểm chính của tài liệu này",
	"Hướng dẫn tôi cách tích hợp API vào hệ thống",
	"Giải thích giúp tôi nguyên lý hoạt động của kiến trúc microservices",
	"Gợi ý cho tôi một số giải pháp tối ưu hóa hiệu năng",
	"Chào bạn",
}

// MintJWT sinh JWT HS256 nội bộ với role super_admin.
func MintJWT(secret string, ttlSeconds int64) (string, error) {
	headerJSON := `{"alg":"HS256","typ":"JWT"}`
	headerEnc := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))

	payload := map[string]any{
		"roles": []string{"super_admin"},
		"exp":   time.Now().Unix() + ttlSeconds,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	payloadEnc := base64.RawURLEncoding.EncodeToString(payloadBytes)

	signingInput := headerEnc + "." + payloadEnc
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sigEnc := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + sigEnc, nil
}

type User struct {
	ID    string
	Token string
}

type UserPool struct {
	users []User
	idx   uint64
}

func NewUserPool(users []User) *UserPool {
	return &UserPool{users: users}
}

func (p *UserPool) Len() int {
	return len(p.users)
}

func (p *UserPool) Next() (string, string) {
	if len(p.users) == 0 {
		return "", ""
	}
	i := atomic.AddUint64(&p.idx, 1) - 1
	u := p.users[i%uint64(len(p.users))]
	return u.ID, u.Token
}

// LoadUsers nạp danh sách user từ file CSV, nhận diện linh hoạt các cột token và user_id.
func LoadUsers(csvPath string) ([]User, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%s: file rỗng", csvPath)
	}

	header := records[0]
	tokenCol := -1
	userCol := -1
	for i, h := range header {
		clean := strings.ToLower(strings.TrimSpace(h))
		if clean == "token" || clean == "user_token" || clean == "access_token" || clean == "api_key" {
			if tokenCol == -1 {
				tokenCol = i
			}
		}
		if clean == "user_id" || clean == "userid" || clean == "id" || clean == "user" {
			if userCol == -1 {
				userCol = i
			}
		}
	}

	if tokenCol == -1 {
		return nil, fmt.Errorf("%s: không tìm thấy cột chứa token (token hoặc user_token)", csvPath)
	}

	var users []User
	for i, row := range records[1:] {
		if tokenCol >= len(row) {
			continue
		}
		tok := strings.TrimSpace(row[tokenCol])
		if tok == "" {
			continue
		}
		uid := fmt.Sprintf("%d", i+1)
		if userCol != -1 && userCol < len(row) {
			val := strings.TrimSpace(row[userCol])
			if val != "" {
				uid = val
			}
		}
		users = append(users, User{ID: uid, Token: tok})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%s: không có dòng nào hợp lệ", csvPath)
	}
	return users, nil
}

// ChatSSEScenario kịch bản kiểm thử API Chatbot streaming SSE.
type ChatSSEScenario struct {
	url       string
	headers   map[string]string
	timeout   time.Duration
	pool      *UserPool
	queries   []string
	cacheBust bool
}

func NewChatSSEScenario(
	url string,
	headers map[string]string,
	timeout time.Duration,
	pool *UserPool,
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
	if len(queries) == 0 {
		queries = DefaultQueries
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
	return nil
}

func (s *ChatSSEScenario) Execute(ctx context.Context, client *http.Client) Record {
	q := s.queries[rand.Intn(len(s.queries))]
	if s.cacheBust {
		q = fmt.Sprintf("%s (ma tham chieu %d)", q, rand.Intn(900000)+100000)
	}
	uid, token := s.pool.Next()

	payload := map[string]any{
		"user_token": token,
		"summary":    "",
		"history": []map[string]string{
			{"role": "user", "text": "Xin chào"},
			{"role": "assistant", "text": "Chào bạn, mình là chatbot. Bạn cần hỗ trợ gì?"},
			{"role": "user", "text": q},
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	endSeen := false
	rec := Record{
		Type:    "req",
		TStart:  tStart,
		UserID:  uid,
		EndSeen: &endSeen,
		Ok:      false,
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

	resp, err := client.Do(httpReq)
	if err != nil {
		rec.Err = strPtr(err.Error())
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		return rec
	}
	defer resp.Body.Close()

	rec.Status = &resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		rec.Err = strPtr(fmt.Sprintf("http_%d", resp.StatusCode))
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		return rec
	}

	reader := bufio.NewReader(resp.Body)
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
			if err != io.EOF && !errors.Is(err, context.Canceled) {
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
