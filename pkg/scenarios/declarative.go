package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
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
	"sync/atomic"
	"text/template"
	"time"
)

// DeclarativeConfig định nghĩa toàn bộ kịch bản kiểm thử tải bằng JSON.
// Người dùng khai báo request, headers, template body và các dataset theo cơ chế Generic Datasets.
type DeclarativeConfig struct {
	Name           string                   `json:"name"`               // Tên kịch bản (ví dụ: my-api-test)
	Type           string                   `json:"type"`               // "http" (REST thông thường) hoặc "sse" (Streaming LLM Chatbot)
	URL            string                   `json:"url"`                // URL endpoint nhận tải (hỗ trợ template)
	Method         string                   `json:"method"`             // GET, POST, PUT, DELETE,...
	Headers        map[string]string        `json:"headers"`            // HTTP Headers (hỗ trợ template)
	Body           string                   `json:"body"`               // Chuỗi body template
	BodyFile       string                   `json:"body_file"`          // Đường dẫn file body template
	Datasets       map[string]DatasetConfig `json:"datasets,omitempty"` // Bộ cấp dữ liệu đa nguồn độc lập (Generic Datasets / Data Feeders)
	ExpectedStatus []int                    `json:"expected_status"`    // Danh sách HTTP status hợp lệ (mặc định [200, 201])
	TimeoutSeconds float64                  `json:"timeout_seconds"`    // Read timeout (giây)
	CacheBust      bool                     `json:"cache_bust"`         // Thêm chuỗi ngẫu nhiên chống cache vào URL
	SSE            SSEOptions               `json:"sse"`                // Cấu hình riêng cho kịch bản SSE Streaming
}

// DatasetConfig cấu hình cho một nguồn dữ liệu nạp từ file.
type DatasetConfig struct {
	File   string `json:"file"`             // Đường dẫn tới file dữ liệu (tương đối theo thư mục scenario hoặc tuyệt đối)
	Format string `json:"format,omitempty"` // Định dạng: "csv", "lines" (hoặc "txt"), "json", "jsonl" (mặc định tự nhận diện từ extension)
	Mode   string `json:"mode,omitempty"`   // Chế độ nạp: "random" (ngẫu nhiên, mặc định) hoặc "sequential" (tuần tự xoay vòng)
}

type SSEOptions struct {
	EndMarker   string `json:"end_marker"`   // Ký hiệu kết thúc stream (ví dụ "[DONE]")
	EventFilter string `json:"event_filter"` // Lọc event name (ví dụ "message")
}

// TextLine biểu diễn một dòng văn bản từ dataset kiểu lines/txt.
// Triển khai fmt.Stringer để trong Go template có thể gọi trực tiếp {{.dataset}} hoặc {{.dataset.Line}}.
type TextLine struct {
	Line string
}

func (t TextLine) String() string {
	return t.Line
}

// Dataset đại diện cho một kho dữ liệu đã nạp vào RAM và hỗ trợ cấp phát an toàn đa luồng.
type Dataset struct {
	Name     string
	FilePath string
	Format   string
	Mode     string
	Records  []any
	idx      uint64
}

// Next trả về bản ghi tiếp theo dựa trên Mode (sequential hoặc random), an toàn đồng thời lock-free.
func (d *Dataset) Next() any {
	if len(d.Records) == 0 {
		return nil
	}
	if d.Mode == "sequential" {
		i := atomic.AddUint64(&d.idx, 1) - 1
		return d.Records[i%uint64(len(d.Records))]
	}
	return d.Records[mrand.Intn(len(d.Records))]
}

// DeclarativeScenario triển khai interface Scenario.
type DeclarativeScenario struct {
	cfg         DeclarativeConfig
	dir         string
	headers     map[string]string
	datasets    map[string]*Dataset
	timeout     time.Duration
	urlTmpl     *template.Template
	bodyTmpl    *template.Template
	headerTmpls map[string]*template.Template
	statusMap   map[int]bool
}

// Các hàm bổ trợ sẵn có trong template engine
var defaultFuncMap = template.FuncMap{
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	},
	"quote": func(v any) string {
		s := fmt.Sprint(v)
		b, err := json.Marshal(s)
		if err != nil {
			return fmt.Sprintf("%q", s)
		}
		return string(b)
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
}

func normalizeMapKeys(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)*2)
	for k, v := range m {
		out[k] = v
		lower := strings.ToLower(k)
		if _, exists := out[lower]; !exists {
			out[lower] = v
		}
		if nestedMap, ok := v.(map[string]any); ok {
			out[k] = normalizeMapKeys(nestedMap)
		}
	}
	return out
}

func loadCSVDataset(name string, filePath string) ([]any, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': không mở được file %s: %w", name, filePath, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	allRows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': lỗi cú pháp CSV trong %s: %w", name, filePath, err)
	}
	if len(allRows) == 0 {
		return nil, fmt.Errorf("dataset '%s': file CSV rỗng (%s)", name, filePath)
	}
	if len(allRows) == 1 {
		return nil, fmt.Errorf("dataset '%s': file CSV chỉ có header mà không có dữ liệu (%s)", name, filePath)
	}

	headers := make([]string, len(allRows[0]))
	for i, h := range allRows[0] {
		headers[i] = strings.TrimSpace(h)
	}

	var records []any
	for _, row := range allRows[1:] {
		if len(row) == 0 {
			continue
		}
		allEmpty := true
		for _, v := range row {
			if strings.TrimSpace(v) != "" {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			continue
		}

		rowMap := make(map[string]string)
		for i, val := range row {
			if i < len(headers) {
				h := headers[i]
				v := strings.TrimSpace(val)
				rowMap[h] = v
				rowMap[strings.ToLower(h)] = v
				rowMap[strings.ToUpper(h)] = v
			}
		}
		records = append(records, rowMap)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("dataset '%s': không tìm thấy dòng dữ liệu nào hợp lệ trong %s", name, filePath)
	}
	return records, nil
}

func loadLinesDataset(name string, filePath string) ([]any, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': không mở được file %s: %w", name, filePath, err)
	}

	var records []any
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		records = append(records, TextLine{Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dataset '%s': lỗi đọc file %s: %w", name, filePath, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("dataset '%s': file không chứa dòng dữ liệu nào (%s)", name, filePath)
	}
	return records, nil
}

func loadJSONLDataset(name string, filePath string) ([]any, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': không mở được file %s: %w", name, filePath, err)
	}

	var records []any
	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var val any
		if err := json.Unmarshal([]byte(line), &val); err != nil {
			return nil, fmt.Errorf("dataset '%s': JSON không hợp lệ tại dòng %d trong %s: %w", name, lineNum, filePath, err)
		}
		if m, ok := val.(map[string]any); ok {
			records = append(records, normalizeMapKeys(m))
		} else {
			records = append(records, val)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dataset '%s': lỗi đọc file %s: %w", name, filePath, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("dataset '%s': file JSONL không chứa bản ghi nào (%s)", name, filePath)
	}
	return records, nil
}

func loadJSONDataset(name string, filePath string) ([]any, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': không mở được file %s: %w", name, filePath, err)
	}

	var root any
	if err := json.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("dataset '%s': JSON không hợp lệ trong %s: %w", name, filePath, err)
	}

	var records []any
	switch v := root.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				records = append(records, normalizeMapKeys(m))
			} else {
				records = append(records, item)
			}
		}
	case map[string]any:
		records = append(records, normalizeMapKeys(v))
	default:
		records = append(records, v)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("dataset '%s': file JSON không chứa bản ghi nào (%s)", name, filePath)
	}
	return records, nil
}

// resolveFilePath phân giải đường dẫn file hỗ trợ đầy đủ:
// 1. Đường dẫn tuyệt đối (vd: /var/data/users.csv)
// 2. Ký tự thư mục home '~' (vd: ~/data/users.csv)
// 3. Biến môi trường hệ thống (vd: $DATA_DIR/users.csv, ${DATA_DIR}/items.json)
// 4. Đường dẫn tương đối (vd: queries.txt, ./data/users.csv, ../shared/items.json)
//    - Ưu tiên 1: Tương đối theo thư mục kịch bản baseDir
//    - Ưu tiên 2: Tương đối theo thư mục làm việc hiện tại (working directory)
func resolveFilePath(rawPath string, baseDir string) (string, error) {
	if strings.TrimSpace(rawPath) == "" {
		return "", errors.New("đường dẫn file rỗng")
	}

	// 1. Phân giải biến môi trường
	p := os.ExpandEnv(strings.TrimSpace(rawPath))

	// 2. Phân giải ký tự home directory '~'
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = home
		}
	} else if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}

	p = filepath.Clean(p)

	// 3. Nếu là đường dẫn tuyệt đối (Absolute path)
	if filepath.IsAbs(p) {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("không tìm thấy file tuyệt đối: %s", p)
	}

	// 4. Nếu là đường dẫn tương đối (Relative path)
	// Ưu tiên 1: Tương đối theo thư mục kịch bản (baseDir)
	if baseDir != "" {
		candidate := filepath.Clean(filepath.Join(baseDir, p))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	// Ưu tiên 2: Tương đối theo thư mục làm việc hiện tại (cwd)
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Clean(filepath.Join(cwd, p))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	// Không tìm thấy ở cả hai vị trí
	var tested []string
	if baseDir != "" {
		tested = append(tested, filepath.Clean(filepath.Join(baseDir, p)))
	}
	if cwd, err := os.Getwd(); err == nil {
		tested = append(tested, filepath.Clean(filepath.Join(cwd, p)))
	}
	return "", fmt.Errorf("không tìm thấy file tương đối '%s' (đã kiểm tra: %s)", rawPath, strings.Join(tested, ", "))
}

func loadDataset(name string, cfg DatasetConfig, baseDir string) (*Dataset, error) {
	if cfg.File == "" {
		return nil, fmt.Errorf("dataset '%s': thiếu trường 'file'", name)
	}

	filePath, err := resolveFilePath(cfg.File, baseDir)
	if err != nil {
		return nil, fmt.Errorf("dataset '%s': %w", name, err)
	}

	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	if format == "" {
		ext := strings.ToLower(filepath.Ext(filePath))
		switch ext {
		case ".csv":
			format = "csv"
		case ".txt":
			format = "lines"
		case ".jsonl", ".ndjson":
			format = "jsonl"
		case ".json":
			format = "json"
		default:
			format = "lines"
		}
	}
	if format == "txt" {
		format = "lines"
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode != "sequential" && mode != "seq" {
		mode = "random"
	}

	var records []any

	switch format {
	case "csv":
		records, err = loadCSVDataset(name, filePath)
	case "lines":
		records, err = loadLinesDataset(name, filePath)
	case "jsonl":
		records, err = loadJSONLDataset(name, filePath)
	case "json":
		records, err = loadJSONDataset(name, filePath)
	default:
		records, err = loadLinesDataset(name, filePath)
	}

	if err != nil {
		return nil, err
	}

	return &Dataset{
		Name:     name,
		FilePath: filePath,
		Format:   format,
		Mode:     mode,
		Records:  records,
	}, nil
}

// LoadDeclarativeScenario đọc file JSON hoặc thư mục chứa scenario.json để khởi tạo kịch bản động.
func LoadDeclarativeScenario(targetPath string, params ScenarioParams) (*DeclarativeScenario, error) {
	var jsonPath string
	var baseDir string

	targetPath = os.ExpandEnv(strings.TrimSpace(targetPath))
	if targetPath == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			targetPath = home
		}
	} else if strings.HasPrefix(targetPath, "~/") || strings.HasPrefix(targetPath, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			targetPath = filepath.Join(home, targetPath[2:])
		}
	}
	targetPath = filepath.Clean(targetPath)

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

	// Nạp Body từ file nếu được chỉ định (hỗ trợ cả relative và absolute path)
	if cfg.BodyFile != "" {
		bPath, err := resolveFilePath(cfg.BodyFile, baseDir)
		if err != nil {
			return nil, fmt.Errorf("body_file: %w", err)
		}
		bData, err := os.ReadFile(bPath)
		if err != nil {
			return nil, fmt.Errorf("body_file '%s': %w", bPath, err)
		}
		cfg.Body = string(bData)
	}

	// Parse templates
	urlTmpl, err := template.New("url").Funcs(defaultFuncMap).Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("lỗi cú pháp template trong 'url': %w", err)
	}

	var bodyTmpl *template.Template
	if cfg.Body != "" {
		bodyTmpl, err = template.New("body").Funcs(defaultFuncMap).Parse(cfg.Body)
		if err != nil {
			return nil, fmt.Errorf("lỗi cú pháp template trong 'body': %w", err)
		}
	}

	headerTmpls := make(map[string]*template.Template)
	for k, v := range cfg.Headers {
		t, err := template.New(k).Funcs(defaultFuncMap).Parse(v)
		if err != nil {
			return nil, fmt.Errorf("lỗi cú pháp template trong header '%s': %w", k, err)
		}
		headerTmpls[k] = t
	}
	for k, v := range params.Headers {
		t, err := template.New(k).Funcs(defaultFuncMap).Parse(v)
		if err != nil {
			return nil, fmt.Errorf("lỗi cú pháp template trong CLI header '%s': %w", k, err)
		}
		headerTmpls[k] = t
	}

	// Chuẩn bị danh mục Datasets
	if cfg.Datasets == nil {
		cfg.Datasets = make(map[string]DatasetConfig)
	}

	// Quét đăng ký dataset động từ CLI (-opt dataset.<name>.file / -opt dataset.<name>.mode)
	for k, v := range params.Options {
		if strings.HasPrefix(k, "dataset.") && strings.HasSuffix(k, ".file") {
			parts := strings.Split(k, ".")
			if len(parts) == 3 {
				dsName := parts[1]
				dCfg := cfg.Datasets[dsName]
				dCfg.File = v
				if optFmt := params.GetOpt("dataset."+dsName+".format", ""); optFmt != "" {
					dCfg.Format = optFmt
				}
				if optMode := params.GetOpt("dataset."+dsName+".mode", ""); optMode != "" {
					dCfg.Mode = optMode
				}
				cfg.Datasets[dsName] = dCfg
			}
		}
	}

	// Kiểm tra ghi đè từng dataset đã khai báo từ CLI (-opt dataset.<name>.file / -opt <name>=file)
	for name, dCfg := range cfg.Datasets {
		if optFile := params.GetOpt("dataset."+name+".file", params.GetOpt(name, "")); optFile != "" {
			dCfg.File = optFile
		}
		if optMode := params.GetOpt("dataset."+name+".mode", ""); optMode != "" {
			dCfg.Mode = optMode
		}
		if optFmt := params.GetOpt("dataset."+name+".format", ""); optFmt != "" {
			dCfg.Format = optFmt
		}
		cfg.Datasets[name] = dCfg
	}

	// Nạp tất cả datasets vào bộ nhớ
	datasets := make(map[string]*Dataset)
	for name, dCfg := range cfg.Datasets {
		ds, err := loadDataset(name, dCfg, baseDir)
		if err != nil {
			return nil, err
		}
		datasets[name] = ds
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
		datasets:    datasets,
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
	if len(s.datasets) > 0 {
		fmt.Printf("datasets : %d nguồn dữ liệu đã nạp:\n", len(s.datasets))
		for name, ds := range s.datasets {
			fmt.Printf("  - %-12s: %d bản ghi (định dạng: %s, chế độ: %s, file: %s)\n",
				name, len(ds.Records), ds.Format, ds.Mode, filepath.Base(ds.FilePath))
		}
	}
	return nil
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *DeclarativeScenario) renderTmpl(t *template.Template, data any) string {
	if t == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return ""
	}
	return buf.String()
}

func (s *DeclarativeScenario) Execute(ctx context.Context, client *http.Client) Record {
	tmplData := make(map[string]any, len(s.datasets)+5)

	// Các biến động hệ thống sinh ngẫu nhiên cho mỗi request
	tmplData["RandomInt"] = mrand.Intn(900000) + 100000
	tmplData["Timestamp"] = time.Now().Unix()
	tmplData["TimestampNano"] = time.Now().UnixNano()
	tmplData["UUID"] = randomUUID()

	// Trích xuất bản ghi tiếp theo từ mỗi Dataset đã khai báo
	for name, ds := range s.datasets {
		tmplData[name] = ds.Next()
	}

	// Cung cấp trường "Data" tự tham chiếu cho phép cú pháp {{.Data.dataset.field}}
	tmplData["Data"] = tmplData

	finalURL := s.renderTmpl(s.urlTmpl, tmplData)
	if finalURL == "" {
		finalURL = s.cfg.URL
	}

	if s.cfg.CacheBust {
		separator := "?"
		if strings.Contains(finalURL, "?") {
			separator = "&"
		}
		finalURL = fmt.Sprintf("%s%s_cb=%d", finalURL, separator, time.Now().UnixNano())
	}

	finalBody := s.renderTmpl(s.bodyTmpl, tmplData)

	tStart := float64(time.Now().UnixNano()) / 1e9
	t0 := time.Now()

	endSeen := false
	rec := Record{
		Type:     "req",
		TStart:   tStart,
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
		val := s.renderTmpl(tmpl, tmplData)
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
