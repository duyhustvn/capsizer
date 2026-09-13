package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ramps/golang/scenarios"
)

// ==============================================================================
// CẤU TRÚC DỮ LIỆU ĐIỀU PHỐI (RUNNER ENGINE DATA STRUCTURES)
// ==============================================================================

type RunMeta struct {
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	Scenario    string    `json:"scenario"`
	Steps       []float64 `json:"steps"`
	StepSeconds float64   `json:"step_seconds"`
}

type StepBoundary struct {
	Type string  `json:"type"`
	RPS  float64 `json:"rps"`
	T    float64 `json:"t"`
}

// ==============================================================================
// TIỆN ÍCH THỜI GIAN & TOÁN HỌC
// ==============================================================================

func unixTime(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func percentile(xs []float64, p float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	k := int(math.Round((p / 100.0) * float64(len(s)-1)))
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	val := s[k]
	return &val
}

func fmtFloatPtr(v *float64, nd int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.*f", nd, *v)
}

// ==============================================================================
// HEADER FLAG PARSER (Hỗ trợ -H và --header lặp lại nhiều lần)
// ==============================================================================

type headerList []string

func (h *headerList) String() string {
	return strings.Join(*h, ", ")
}

func (h *headerList) Set(val string) error {
	*h = append(*h, val)
	return nil
}

// ==============================================================================
// STEP RUNNER & OPEN-LOOP DISPATCHER
// ==============================================================================

type StepResult struct {
	RpsOffered         float64
	Sent               int
	DroppedByGenerator int
	CompletedOk        int
	Errors             int
	RpsAchieved        float64
	P50                *float64
	P95                *float64
	MaxInflight        int
}

func runStep(
	ctx context.Context,
	client *http.Client,
	scenario scenarios.Scenario,
	rps float64,
	stepSeconds float64,
	settleSeconds float64,
	maxInflight int,
	outWriter *bufio.Writer,
) (StepResult, error) {
	interval := time.Duration(float64(time.Second) / rps)

	tStepStart := unixTime(time.Now())
	startBoundary, _ := json.Marshal(StepBoundary{Type: "step_start", RPS: rps, T: tStepStart})
	_, _ = outWriter.Write(append(startBoundary, '\n'))
	_ = outWriter.Flush()

	var inflight atomic.Int64
	var maxInflightSeen atomic.Int64
	var dropped atomic.Int64

	recordsChan := make(chan scenarios.Record, maxInflight*2+1000)
	var wg sync.WaitGroup

	stepDeadline := time.Now().Add(time.Duration(stepSeconds * float64(time.Second)))
	n := int64(0)
	startTime := time.Now()

	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()

	// Thu thập kết quả bất đồng bộ từ các goroutine
	var records []scenarios.Record
	collectorDone := make(chan struct{})
	go func() {
		for r := range recordsChan {
			records = append(records, r)
		}
		close(collectorDone)
	}()

	for {
		select {
		case <-ctx.Done():
			break
		default:
		}

		targetTime := startTime.Add(time.Duration(float64(n) * float64(interval)))
		now := time.Now()
		if targetTime.After(stepDeadline) || now.After(stepDeadline) {
			break
		}

		delay := targetTime.Sub(now)
		if delay > 0 {
			time.Sleep(delay)
		}
		n++

		curInflight := inflight.Load()
		if curInflight >= int64(maxInflight) {
			dropped.Add(1)
			continue
		}

		inflight.Add(1)
		for {
			curMax := maxInflightSeen.Load()
			if curInflight+1 <= curMax || maxInflightSeen.CompareAndSwap(curMax, curInflight+1) {
				break
			}
		}

		wg.Add(1)
		go func() {
			defer func() {
				inflight.Add(-1)
				wg.Done()
			}()

			rec := scenario.Execute(stepCtx, client)
			recordsChan <- rec
		}()
	}

	// Giai đoạn thu hồi (settle/drain) các request dở dang cuối bước
	settleDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(settleDone)
	}()

	select {
	case <-settleDone:
	case <-time.After(time.Duration(settleSeconds * float64(time.Second))):
		// Hết thời gian chờ settle, hủy context để ngắt kết nối dở dang
		cancelStep()
		// Đợi thêm tối đa 5 giây cho dọn dẹp
		select {
		case <-settleDone:
		case <-time.After(5 * time.Second):
		}
	}

	close(recordsChan)
	<-collectorDone

	tStepEnd := unixTime(time.Now())

	// Ghi tất cả request records ra JSONL
	for _, r := range records {
		line, _ := json.Marshal(r)
		_, _ = outWriter.Write(append(line, '\n'))
	}
	endBoundary, _ := json.Marshal(StepBoundary{Type: "step_end", RPS: rps, T: tStepEnd})
	_, _ = outWriter.Write(append(endBoundary, '\n'))
	_ = outWriter.Flush()

	var okRecords []scenarios.Record
	var metrics []float64
	for _, r := range records {
		if r.Ok {
			okRecords = append(okRecords, r)
			if r.TtftMs != nil {
				metrics = append(metrics, *r.TtftMs)
			}
		}
	}

	completedOk := len(okRecords)
	errorsCount := len(records) - completedOk
	rpsAchieved := float64(completedOk) / math.Max(stepSeconds, 1e-9)

	return StepResult{
		RpsOffered:         rps,
		Sent:               int(n - dropped.Load()),
		DroppedByGenerator: int(dropped.Load()),
		CompletedOk:        completedOk,
		Errors:             errorsCount,
		RpsAchieved:        rpsAchieved,
		P50:                percentile(metrics, 50),
		P95:                percentile(metrics, 95),
		MaxInflight:        int(maxInflightSeen.Load()),
	}, nil
}

// ==============================================================================
// MAIN PROGRAM
// ==============================================================================

func main() {
	var (
		scenarioName = flag.String("scenario", "chat-sse", "loại kịch bản: 'chat-sse' (mặc định) hoặc 'rest'")
		method       = flag.String("method", "GET", "HTTP method cho kịch bản REST (GET, POST, PUT, DELETE,...)")
		body         = flag.String("body", "", "chuỗi body gửi kèm cho request REST (chuỗi JSON hoặc chuỗi text thô)")
		bodyFile     = flag.String("body-file", "", "đường dẫn file chứa dữ liệu body cho request REST")
		targetURL    = flag.String("url", "http://127.0.0.1:8000/chat", "URL endpoint nhận request")
		stepsStr     = flag.String("steps", "1,2,4,8,16,32", "danh sách RPS, cách nhau dấu phẩy")
		stepSeconds  = flag.Float64("step-seconds", 60.0, "độ dài mỗi bậc (giây)")
		cooldown     = flag.Float64("cooldown", 20.0, "nghỉ giữa hai bậc (giây)")
		settle       = flag.Float64("settle", 60.0, "chờ tối đa cho in-flight cuối bậc (giây)")
		timeout      = flag.Float64("timeout", 120.0, "read timeout mỗi request (giây)")
		maxInflight  = flag.Int("max-inflight", 2000, "trần in-flight của MÁY BẮN TẢI")
		usersCSV     = flag.String("users-csv", "", "CSV pool user (chứa cột token/user_token). Mặc định users.csv nếu tồn tại.")
		userToken    = flag.String("user-token", "loadtest-token", "token dùng chung khi KHÔNG có --users-csv")
		jwtToken     = flag.String("jwt", "", "JWT dựng sẵn")
		jwtSecret    = flag.String("jwt-secret", "", "secret ký token HS256; mặc định đọc biến môi trường JWT_SECRET")
		queriesFile  = flag.String("queries", "", "file câu hỏi cho chatbot, mỗi dòng một câu")
		insecure     = flag.Bool("insecure", false, "bỏ qua verify chứng chỉ TLS (khi qua Ingress cert tự ký)")
		cacheBust    = flag.Bool("cache-bust", false, "thêm mã ngẫu nhiên vào câu hỏi để tránh cache")
		stopOnKnee   = flag.Bool("stop-on-knee", false, "dừng khi achieved < 90% offered")
		outFile      = flag.String("out", "loadtest-ramp.jsonl", "file JSONL xuất kết quả kiểm thử tải")
	)

	var headersFlag headerList
	flag.Var(&headersFlag, "H", "thêm HTTP header tùy chọn (ví dụ: -H 'X-Api-Key: 123')")
	flag.Var(&headersFlag, "header", "thêm HTTP header tùy chọn")
	flag.Parse()

	if *jwtSecret == "" {
		*jwtSecret = os.Getenv("JWT_SECRET")
	}

	// Parse danh sách steps
	var steps []float64
	for _, s := range strings.Split(*stepsStr, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: giá trị RPS không hợp lệ: %s\n", s)
			os.Exit(2)
		}
		steps = append(steps, v)
	}
	if len(steps) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --steps rỗng")
		os.Exit(2)
	}

	// Parse headers thành map
	headers := make(map[string]string)
	for _, h := range headersFlag {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	// Xử lý JWT Authorization Header
	token := *jwtToken
	if token == "" && *jwtSecret != "" {
		minted, err := scenarios.MintJWT(*jwtSecret, 7200)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: ký JWT thất bại: %v\n", err)
			os.Exit(2)
		}
		token = minted
	}
	if token != "" {
		if _, exists := headers["Authorization"]; !exists {
			headers["Authorization"] = "Bearer " + token
		}
	}

	// Khởi tạo Scenario tương ứng từ package scenarios
	var scenario scenarios.Scenario
	if *scenarioName == "rest" {
		var bodyData []byte
		if *body != "" {
			bodyData = []byte(*body)
		} else if *bodyFile != "" {
			content, err := os.ReadFile(*bodyFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: đọc file body thất bại: %v\n", err)
				os.Exit(2)
			}
			bodyData = content
		}

		if len(bodyData) > 0 {
			var js json.RawMessage
			if json.Unmarshal(bodyData, &js) == nil {
				if _, exists := headers["Content-Type"]; !exists {
					headers["Content-Type"] = "application/json"
				}
			}
		}

		scenario = scenarios.NewRestScenario(
			*targetURL,
			*method,
			headers,
			time.Duration(*timeout*float64(time.Second)),
			bodyData,
		)
	} else {
		// Default: chat-sse
		queries := scenarios.DefaultQueries
		if *queriesFile != "" {
			content, err := os.ReadFile(*queriesFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: đọc file queries thất bại: %v\n", err)
				os.Exit(2)
			}
			var lines []string
			for _, line := range strings.Split(string(content), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					lines = append(lines, line)
				}
			}
			if len(lines) == 0 {
				fmt.Fprintln(os.Stderr, "ERROR: file --queries rỗng")
				os.Exit(2)
			}
			queries = lines
		}

		csvFile := *usersCSV
		if csvFile == "" {
			if _, err := os.Stat("users.csv"); err == nil {
				csvFile = "users.csv"
			}
		}

		var pool *scenarios.UserPool
		if csvFile != "" {
			users, err := scenarios.LoadUsers(csvFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "CẢNH BÁO: không đọc được file %s (%v), chuyển sang dùng --user-token\n", csvFile, err)
				pool = scenarios.NewUserPool([]scenarios.User{{ID: "", Token: *userToken}})
				fmt.Println("pool     : 1 token dùng chung (--user-token)")
			} else {
				pool = scenarios.NewUserPool(users)
				fmt.Printf("pool     : %d user từ %s\n", len(users), filepath.Base(csvFile))
			}
		} else {
			pool = scenarios.NewUserPool([]scenarios.User{{ID: "", Token: *userToken}})
			fmt.Println("pool     : 1 token dùng chung (--user-token)")
		}

		scenario = scenarios.NewChatSSEScenario(
			*targetURL,
			headers,
			time.Duration(*timeout*float64(time.Second)),
			pool,
			queries,
			*cacheBust,
		)
	}

	// Đảm bảo thư mục lưu output tồn tại
	if err := os.MkdirAll(filepath.Dir(*outFile), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: không thể tạo thư mục cho %s: %v\n", *outFile, err)
		os.Exit(2)
	}
	f, err := os.Create(*outFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: không thể mở file xuất kết quả: %v\n", err)
		os.Exit(2)
	}
	defer f.Close()
	outWriter := bufio.NewWriterSize(f, 256*1024)

	// Ghi bản ghi run_meta
	runMetaBytes, _ := json.Marshal(RunMeta{
		Type:        "run_meta",
		URL:         *targetURL,
		Scenario:    scenario.Name(),
		Steps:       steps,
		StepSeconds: *stepSeconds,
	})
	_, _ = outWriter.Write(append(runMetaBytes, '\n'))
	_ = outWriter.Flush()

	fmt.Printf("target   : %s\n", *targetURL)
	fmt.Printf("scenario : %s\n", scenario.Name())
	fmt.Printf("steps    : %v x %.0fs (settle %.0fs)\n", steps, *stepSeconds, *settle)
	fmt.Printf("out      : %s\n", *outFile)

	p50Lbl := fmt.Sprintf("%s_p50", scenario.MetricLabel())
	p95Lbl := fmt.Sprintf("%s_p95", scenario.MetricLabel())
	hdr := fmt.Sprintf("%8s %9s %6s %5s %5s %9s %9s %9s",
		"offered", "achieved", "ok", "err", "drop", p50Lbl, p95Lbl, "inflight")
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	// Cấu hình HTTP client hiệu năng cao với Connection Pooling lớn
	capConn := *maxInflight + 64
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        capConn,
		MaxIdleConnsPerHost: capConn,
		MaxConnsPerHost:     capConn,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: *insecure,
		},
	}
	client := &http.Client{
		Transport: transport,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nNhận tín hiệu dừng (Ctrl+C). Đang hoàn tất...")
		cancel()
	}()

	if err := scenario.Setup(ctx, client); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: setup kịch bản thất bại: %v\n", err)
		os.Exit(2)
	}

	for i, rps := range steps {
		select {
		case <-ctx.Done():
			break
		default:
		}

		res, err := runStep(ctx, client, scenario, rps, *stepSeconds, *settle, *maxInflight, outWriter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR khi chạy bậc %.1f: %v\n", rps, err)
			break
		}

		fmt.Printf("%8.1f %9.2f %6d %5d %5d %9s %9s %9d\n",
			res.RpsOffered, res.RpsAchieved, res.CompletedOk,
			res.Errors, res.DroppedByGenerator,
			fmtFloatPtr(res.P50, 0), fmtFloatPtr(res.P95, 0), res.MaxInflight)

		if *stopOnKnee && res.RpsAchieved < 0.90*res.RpsOffered {
			fmt.Println("\n-> achieved < 90% offered: đã qua điểm gãy, dừng ramp.")
			break
		}

		if i < len(steps)-1 && *cooldown > 0 {
			select {
			case <-ctx.Done():
				break
			case <-time.After(time.Duration(*cooldown * float64(time.Second))):
			}
		}
	}

	_ = outWriter.Flush()

	fmt.Println("\nXong. Ghép với probe rồi dựng bảng:")
	fmt.Printf("  python3 report.py --ramp %s --probe <probe.jsonl>\n", *outFile)
}
