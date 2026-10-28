package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// DeclarativeConfig định nghĩa toàn bộ kịch bản kiểm thử tải bằng JSON.
// Không cần viết code Go, người dùng chỉ cần viết file scenario.json giống như k6.
type DeclarativeConfig struct {
	Name           string            `json:"name"`                     // Tên kịch bản (ví dụ: my-api-test)
	Type           string            `json:"type"`                     // "http" (REST thông thường) hoặc "sse" (Streaming LLM Chatbot)
	URL            string            `json:"url"`                      // URL endpoint nhận tải (hỗ trợ template)
	Method         string            `json:"method"`                   // GET, POST, PUT, DELETE,...
	Headers        map[string]string `json:"headers"`                  // HTTP Headers (hỗ trợ template)
	Body           string            `json:"body"`                     // Chuỗi body template
	BodyFile       string            `json:"body_file"`                // Đường dẫn file body template
	QueriesFile    string            `json:"queries_file"`             // File danh sách câu hỏi / prompts
	UsersFile      string            `json:"users_file"`               // File users.csv chứa pool token
	ExpectedStatus []int             `json:"expected_status"`          // Danh sách HTTP status hợp lệ (mặc định [200, 201])
	TimeoutSeconds float64           `json:"timeout_seconds"`          // Read timeout (giây)
	CacheBust      bool              `json:"cache_bust"`               // Thêm chuỗi ngẫu nhiên chống cache
	SSE            SSEOptions        `json:"sse"`                      // Cấu hình riêng cho kịch bản SSE Streaming
}

type SSEOptions struct {
	EndMarker   string `json:"end_marker"`   // Ký hiệu kết thúc stream (ví dụ "[DONE]")
	EventFilter string `json:"event_filter"` // Lọc event name (ví dụ "message")
}

// TemplateContext chứa các biến cung cấp cho template mỗi request.
type TemplateContext struct {
	Query     string
	Token     string
	UserID    string
	RandomInt int
	Timestamp int64
	UUID      string
	User      struct {
		Token string
		ID    string
	}
}

// DeclarativeScenario triển khai interface Scenario.
type DeclarativeScenario struct {
	cfg          DeclarativeConfig
	dir          string
	headers      map[string]string
	queries      []string
	pool         *UserPool
	timeout      time.Duration
	urlTmpl      *template.Template
	bodyTmpl     *template.Template
	headerTmpls  map[string]*template.Template
	statusMap    map[int]bool
}

// LoadDeclarativeScenario đọc file JSON hoặc thư mục chứa scenario.json để khởi tạo kịch bản động.
func LoadDeclarativeScenario(targetPath string, params ScenarioParams) (*DeclarativeScenario, error) {
	var jsonPath string
	var baseDir string

	fi, err := os.Stat(targetPath)
	if err != nil {
		return nil, fmt.Errorf("không tìm thấy file hoặc thư mục kịch bản: %s (%w)", targetPath, err)
	}

	if fi.IsDir() {
		baseDir = targetPath
		candidates := []string{
			filepath.Join(targetPath, "scenario.json"),
			filepath.Join(targetPath, "test.json"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				jsonPath = c
				break
			}
		}
		if jsonPath == "" {
			return nil, fmt.Errorf("thư mục '%s' không chứa file scenario.json", targetPath)
		}
	} else {
		jsonPath = targetPath
		baseDir = filepath.Dir(targetPath)
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("lỗi đọc file kịch bản %s: %w", jsonPath, err)
	}

	var cfg DeclarativeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("lỗi cú pháp JSON trong %s: %w", jsonPath, err)
	}

	if cfg.Type == "" {
		cfg.Type = "http"
	}
	cfg.Type = strings.ToLower(cfg.Type)

	if cfg.Method == "" {
		if cfg.Type == "sse" {
			cfg.Method = "POST"
		} else {
			cfg.Method = "GET"
		}
	}

	// Ghi đè URL từ CLI nếu có
	if params.TargetURL != "" && params.TargetURL != "http://127.0.0.1:8000/chat" {
		cfg.URL = params.TargetURL
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("kịch bản '%s' thiếu trường 'url'", jsonPath)
	}

	// Xử lý timeout
	timeout := params.Timeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds * float64(time.Second))
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	// Nạp Body từ file nếu được chỉ định
	if cfg.BodyFile != "" {
		bPath := cfg.BodyFile
		if !filepath.IsAbs(bPath) {
			bPath = filepath.Join(baseDir, bPath)
		}
		bData, err := os.ReadFile(bPath)
		if err == nil {
			cfg.Body = string(bData)
		}
	}

	// Parse templates
	urlTmpl, _ := template.New("url").Parse(cfg.URL)
	var bodyTmpl *template.Template
	if cfg.Body != "" {
		bodyTmpl, _ = template.New("body").Parse(cfg.Body)
	}

	headerTmpls := make(map[string]*template.Template)
	for k, v := range cfg.Headers {
		t, _ := template.New(k).Parse(v)
		headerTmpls[k] = t
	}
	// Gộp thêm headers truyền từ CLI (-H)
	for k, v := range params.Headers {
		t, _ := template.New(k).Parse(v)
		headerTmpls[k] = t
	}

	// Nạp queries
	var queries []string
	if cfg.QueriesFile != "" {
		qPath := cfg.QueriesFile
		if !filepath.IsAbs(qPath) {
			qPath = filepath.Join(baseDir, qPath)
		}
		queries = ResolveQueries(qPath, "")
	}
	if len(queries) == 0 {
		if qOpt := params.GetOpt("queries", params.GetOpt("queries-file", "")); qOpt != "" {
			queries = ResolveQueries(qOpt, "")
		}
	}

	// Nạp users
	var userPool *UserPool
	if cfg.UsersFile != "" {
		uPath := cfg.UsersFile
		if !filepath.IsAbs(uPath) {
			uPath = filepath.Join(baseDir, uPath)
		}
		users, err := LoadUsers(uPath)
		if err == nil && len(users) > 0 {
			userPool = NewUserPool(users)
		}
	}
	if userPool == nil {
		if uOpt := params.GetOpt("users", params.GetOpt("users-file", params.GetOpt("users-csv", ""))); uOpt != "" {
			if users, err := LoadUsers(uOpt); err == nil && len(users) > 0 {
				userPool = NewUserPool(users)
			}
		}
	}
	if userPool == nil {
		token := params.GetOpt("token", "loadtest-token")
		userPool = NewUserPool([]User{{Token: token}})
	}

	// Danh sách status hợp lệ
	statusMap := make(map[int]bool)
	if len(cfg.ExpectedStatus) == 0 {
		statusMap[200] = true
		statusMap[201] = true
	} else {
		for _, s := range cfg.ExpectedStatus {
			statusMap[s] = true
		}
	}

	name := cfg.Name
	if name == "" {
		name = filepath.Base(baseDir)
	}

	return &DeclarativeScenario{
		cfg:         cfg,
		dir:         baseDir,
		headers:     cfg.Headers,
		queries:     queries,
		pool:        userPool,
		timeout:     timeout,
		urlTmpl:     urlTmpl,
		bodyTmpl:    bodyTmpl,
		headerTmpls: headerTmpls,
		statusMap:   statusMap,
	}, nil
}

func (s *DeclarativeScenario) Name() string {
	return s.cfg.Name
}

func (s *DeclarativeScenario) MetricLabel() string {
	if s.cfg.Type == "sse" {
		return "ttft"
	}
	return "lat"
}

func (s *DeclarativeScenario) Target() string {
	return s.cfg.URL
}

func (s *DeclarativeScenario) Setup(ctx context.Context, client *http.Client) error {
	fmt.Printf("[Declarative Scenario: %s] Kiểu: %s | Endpoint: %s\n", s.cfg.Name, strings.ToUpper(s.cfg.Type), s.cfg.URL)
	return nil
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *DeclarativeScenario) renderTmpl(t *template.Template, ctx TemplateContext) string {
	if t == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return ""
	}
	return buf.String()
}

func (s *DeclarativeScenario) Execute(ctx context.Context, client *http.Client) Record {
	q := ""
	if len(s.queries) > 0 {
		q = s.queries[mrand.Intn(len(s.queries))]
	}
	if s.cfg.CacheBust {
		q = fmt.Sprintf("%s (id %d)", q, mrand.Intn(900000)+100000)
	}

	userID, token := s.pool.Next()

	tmplCtx := TemplateContext{
		Query:     q,
		Token:     token,
		UserID:    userID,
		RandomInt: mrand.Intn(900000) + 100000,
		Timestamp: time.Now().Unix(),
		UUID:      randomUUID(),
	}
	tmplCtx.User.Token = token
	tmplCtx.User.ID = userID

	finalURL := s.renderTmpl(s.urlTmpl, tmplCtx)
	if finalURL == "" {
		finalURL = s.cfg.URL
	}

	finalBody := s.renderTmpl(s.bodyTmpl, tmplCtx)

	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	endSeen := false
	rec := Record{
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

	var bodyReader io.Reader
	if len(finalBody) > 0 {
		bodyReader = strings.NewReader(finalBody)
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, s.cfg.Method, finalURL, bodyReader)
	if err != nil {
		rec.Err = StrPtr(err.Error())
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		return rec
	}

	for k, tmpl := range s.headerTmpls {
		val := s.renderTmpl(tmpl, tmplCtx)
		httpReq.Header.Set(k, val)
	}

	if s.cfg.Type == "sse" && httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if len(finalBody) > 0 && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			rec.Err = StrPtr("abandoned")
		} else {
			rec.Err = StrPtr(err.Error())
		}
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		return rec
	}
	defer resp.Body.Close()

	rec.Status = &resp.StatusCode

	if !s.statusMap[resp.StatusCode] {
		rec.Err = StrPtr("http_" + strconv.Itoa(resp.StatusCode))
		elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
		rec.TotalMs = &elapsed
		_, _ = io.Copy(io.Discard, resp.Body)
		return rec
	}

	// 1. Xử lý kiểu SSE Streaming
	if s.cfg.Type == "sse" {
		reader := bufio.NewReaderSize(resp.Body, 64*1024)
		endMarker := s.cfg.SSE.EndMarker
		if endMarker == "" {
			endMarker = "[DONE]"
		}

		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				if strings.HasPrefix(trimmed, "data:") {
					now := float64(time.Since(t0).Microseconds()) / 1000.0
					if rec.TtftMs == nil {
						rec.TtftMs = &now
					}
					dataContent := strings.TrimSpace(trimmed[5:])
					if strings.Contains(dataContent, endMarker) {
						*rec.EndSeen = true
						break
					}
				}
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					if rec.Err == nil {
						rec.Err = StrPtr("abandoned")
					}
				} else if err != io.EOF {
					if rec.Err == nil {
						rec.Err = StrPtr("stream_read_err: " + err.Error())
					}
				}
				break
			}
		}
	} else {
		// 2. Xử lý kiểu HTTP REST thông thường
		buf := make([]byte, 32*1024)
		firstByte := false
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 && !firstByte {
				firstByte = true
				now := float64(time.Since(t0).Microseconds()) / 1000.0
				rec.TtftMs = &now
			}
			if err != nil {
				if err != io.EOF && rec.Err == nil {
					rec.Err = StrPtr("read_err: " + err.Error())
				}
				break
			}
		}
	}

	elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
	rec.TotalMs = &elapsed
	if rec.TtftMs == nil {
		rec.TtftMs = &elapsed
	}
	rec.Ok = (rec.Err == nil)
	return rec
}
