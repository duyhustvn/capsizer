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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultQueries danh sách câu hỏi kiểm thử mẫu chung cho chatbot (dự phòng khi không cấu hình .env).
var DefaultQueries = []string{}

// LoadDotEnv tìm và nạp các biến môi trường từ file .env vào process (os.Setenv) nếu chưa tồn tại.
func LoadDotEnv(path string) map[string]string {
	var candidates []string
	if path != "" {
		candidates = append(candidates, path)
		if !filepath.IsAbs(path) {
			candidates = append(candidates,
				filepath.Join(".", path),
				filepath.Join("..", path),
				filepath.Join("..", "..", path),
			)
		}
	} else {
		candidates = append(candidates,
			".env",
			filepath.Join("..", ".env"),
			filepath.Join("..", "..", ".env"),
		)
	}

	var targetPath string
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			targetPath = c
			break
		}
	}
	if targetPath == "" {
		return nil
	}

	content, err := os.ReadFile(targetPath)
	if err != nil {
		return nil
	}

	envVars := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		i++
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])

		// Xử lý chuỗi multiline nằm trong dấu ngoặc kép hoặc đơn
		if (strings.HasPrefix(v, "\"") && !(strings.HasSuffix(v, "\"") && len(v) > 1)) ||
			(strings.HasPrefix(v, "'") && !(strings.HasSuffix(v, "'") && len(v) > 1)) {
			quoteChar := string(v[0])
			valParts := []string{v[1:]}
			for i < len(lines) {
				nextLine := lines[i]
				i++
				if strings.HasSuffix(strings.TrimRight(nextLine, "\r"), quoteChar) {
					trimmed := strings.TrimRight(nextLine, "\r")
					valParts = append(valParts, trimmed[:len(trimmed)-1])
					break
				}
				valParts = append(valParts, strings.TrimRight(nextLine, "\r"))
			}
			v = strings.Join(valParts, "\n")
		} else if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
			(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
			v = v[1 : len(v)-1]
		}

		v = strings.ReplaceAll(v, "\\n", "\n")
		envVars[k] = v
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
	return envVars
}

// ParseQueries phân tích chuỗi câu hỏi từ .env (hỗ trợ JSON array, xuống dòng, | hoặc ;).
func ParseQueries(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// 1. JSON Array
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			var res []string
			for _, item := range list {
				item = strings.TrimSpace(item)
				if item != "" {
					res = append(res, item)
				}
			}
			if len(res) > 0 {
				return res
			}
		}
	}

	// 2. Phân tách theo dòng (\n)
	if strings.Contains(raw, "\n") {
		var res []string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				res = append(res, line)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	// 3. Phân tách theo dấu pipe (|)
	if strings.Contains(raw, "|") {
		var res []string
		for _, item := range strings.Split(raw, "|") {
			item = strings.TrimSpace(item)
			if item != "" {
				res = append(res, item)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	// 4. Phân tách theo dấu chấm phẩy (;)
	if strings.Contains(raw, ";") {
		var res []string
		for _, item := range strings.Split(raw, ";") {
			item = strings.TrimSpace(item)
			if item != "" {
				res = append(res, item)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	return []string{raw}
}

// ResolveQueries xác định danh sách câu hỏi kiểm thử theo độ ưu tiên:
// 1. Cờ dòng lệnh --queries (file txt/csv)
// 2. Biến CHAT_QUERIES_FILE hoặc QUERIES_FILE từ .env
// 3. Biến CHAT_QUERIES hoặc QUERIES từ .env
// 4. Danh sách mặc định DefaultQueries
func ResolveQueries(queriesFile string, envFile string) []string {
	LoadDotEnv(envFile)

	// 1. Cờ dòng lệnh --queries
	if queriesFile != "" {
		content, err := os.ReadFile(queriesFile)
		if err == nil {
			var lines []string
			for _, line := range strings.Split(string(content), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					lines = append(lines, line)
				}
			}
			if len(lines) > 0 {
				fmt.Printf("queries  : %d câu hỏi nạp từ cờ --queries (%s)\n", len(lines), filepath.Base(queriesFile))
				return lines
			}
		}
		fmt.Fprintf(os.Stderr, "CẢNH BÁO: không đọc được file --queries: %s\n", queriesFile)
	}

	// 2. Biến file từ .env
	qFile := os.Getenv("CHAT_QUERIES_FILE")
	if qFile == "" {
		qFile = os.Getenv("QUERIES_FILE")
	}
	if qFile != "" {
		candidates := []string{
			qFile,
			filepath.Join("..", qFile),
			filepath.Join("..", "..", qFile),
		}
		for _, c := range candidates {
			content, err := os.ReadFile(c)
			if err == nil {
				var lines []string
				for _, line := range strings.Split(string(content), "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						lines = append(lines, line)
					}
				}
				if len(lines) > 0 {
					fmt.Printf("queries  : %d câu hỏi nạp từ file %s (qua .env)\n", len(lines), filepath.Base(c))
					return lines
				}
			}
		}
	}

	// 3. Chuỗi danh sách câu hỏi trong CHAT_QUERIES hoặc QUERIES (.env)
	rawEnv := os.Getenv("CHAT_QUERIES")
	if rawEnv == "" {
		rawEnv = os.Getenv("QUERIES")
	}
	if rawEnv != "" {
		parsed := ParseQueries(rawEnv)
		if len(parsed) > 0 {
			fmt.Printf("queries  : %d câu hỏi nạp từ .env (CHAT_QUERIES)\n", len(parsed))
			return parsed
		}
	}

	// 4. Dự phòng mặc định
	fmt.Println("queries  : dùng danh sách câu hỏi mẫu mặc định (chưa cấu hình CHAT_QUERIES trong .env)")
	return DefaultQueries
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

	// Lượt 2: Tìm kiếm gần đúng (fuzzy match) nếu chưa tìm thấy khớp chính xác.
	// Nhận diện bất kỳ cột nào chứa chuỗi "token" (không phân biệt hoa/thường, ví dụ: "tokenUserChatbot", "auth_token",...).
	// Cơ chế này giúp tự động tương thích với các định dạng CSV từ nhiều nguồn, tránh việc LoadUsers báo lỗi
	// khiến hệ thống tự động rơi về chế độ dùng 1 token dùng chung làm nghẽn rate-limit của tài khoản.
	if tokenCol == -1 {
		for i, h := range header {
			if strings.Contains(strings.ToLower(strings.TrimSpace(h)), "token") {
				tokenCol = i
				break
			}
		}
	}

	if tokenCol == -1 {
		return nil, fmt.Errorf("%s: không tìm thấy cột nào chứa token trong header %v", csvPath, header)
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
			if errors.Is(err, context.Canceled) {
				// Request bị hủy bởi step runner khi hết thời gian settle cuối bậc kiểm thử.
				// Đánh dấu err = "abandoned" để đồng bộ với bản Python; report.py nhận diện chính xác
				// chuỗi lỗi này làm một trong các chỉ số phát hiện điểm gãy quá tải (knee condition).
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
