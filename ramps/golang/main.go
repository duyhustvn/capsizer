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

// strPtrMain trả về con trỏ tới chuỗi, dùng để gán các trường kiểu *string trong scenarios.Record
// khi tạo bản ghi bổ sung (synthetic record) ở package main.
func strPtrMain(s string) *string {
	return &s
}

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[1;32m"
	colorYellow = "\033[1;33m"
	colorBlue   = "\033[1;36m"
)

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
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
	stepIdx int,
	totalSteps int,
	rps float64,
	stepSeconds float64,
	settleSeconds float64,
	maxInflight int,
	outWriter *bufio.Writer,
	statusFilePath string,
	term bool,
) (StepResult, error) {
	interval := time.Duration(float64(time.Second) / rps)

	tStepStart := unixTime(time.Now())
	startBoundary, _ := json.Marshal(StepBoundary{Type: "step_start", RPS: rps, T: tStepStart})
	_, _ = outWriter.Write(append(startBoundary, '\n'))
	_ = outWriter.Flush()

	var inflight atomic.Int64
	var maxInflightSeen atomic.Int64
	var dropped atomic.Int64
	var n atomic.Int64

	var wg sync.WaitGroup

	stepDeadline := time.Now().Add(time.Duration(stepSeconds * float64(time.Second)))
	startTime := time.Now()

	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()

	// Thu thập kết quả request bằng slice được bảo vệ bởi mutex thay cho buffered channel.
	// Cơ chế này đảm bảo an toàn khi hết thời gian settle: cờ collecting sẽ được đặt về false;
	// các goroutine hoàn tất muộn sau deadline sẽ an toàn bỏ qua record thay vì gửi vào channel
	// đã đóng gây panic "send on closed channel" làm sập tiến trình test tải.
	var (
		recMu      sync.Mutex
		records    []scenarios.Record
		collecting = true
	)
	addRecord := func(r scenarios.Record) {
		recMu.Lock()
		defer recMu.Unlock()
		if collecting {
			records = append(records, r)
		}
	}

	// Goroutine cập nhật trạng thái thời gian thực (Zero overhead: chạy nền độc lập mỗi 250ms)
	var phase atomic.Int32 // 0: RAMPING (đang bắn tải), 1: SETTLING (đang chờ request dở dang)
	var settleStartNano atomic.Int64
	doneStatus := make(chan struct{})

	updateStatus := func(now time.Time) {
		curInflight := inflight.Load()
		curDropped := dropped.Load()
		curSent := n.Load() - curDropped
		curPhase := phase.Load()

		if curPhase == 0 {
			elapsed := now.Sub(startTime).Seconds()
			if elapsed > stepSeconds {
				elapsed = stepSeconds
			}
			remain := stepSeconds - elapsed
			if remain < 0 {
				remain = 0
			}

			if term {
				fmt.Fprintf(os.Stdout, "\r\033[K%s[▶ BẮN TẢI - ĐO TÀI NGUYÊN]%s Bậc %d/%d (%.1f rps) | Đã chạy: %.0fs/%.0fs (còn %.0fs) | Inflight: %d | Đã phát: %d",
					colorGreen, colorReset, stepIdx, totalSteps, rps, elapsed, stepSeconds, remain, curInflight, curSent)
			}
			if statusFilePath != "" {
				_ = os.WriteFile(statusFilePath, []byte(fmt.Sprintf(
					"PHASE=RAMPING\nSTEP=%d/%d\nRPS=%.1f\nELAPSED=%.1fs\nDURATION=%.0fs\nREMAIN=%.1fs\nINFLIGHT=%d\nSENT=%d\nDROPPED=%d\nSHOULD_COLLECT=YES\nACTION=DANG_BAN_TAI_THU_THAP_NGAY\n",
					stepIdx, totalSteps, rps, elapsed, stepSeconds, remain, curInflight, curSent, curDropped)), 0644)
			}
		} else {
			sNano := settleStartNano.Load()
			elapsedSettle := 0.0
			if sNano > 0 {
				elapsedSettle = float64(now.UnixNano()-sNano) / 1e9
			}
			remainSettle := settleSeconds - elapsedSettle
			if remainSettle < 0 {
				remainSettle = 0
			}

			if term {
				fmt.Fprintf(os.Stdout, "\r\033[K%s[⏳ CHỜ SETTLE - CÒN REQUEST]%s Bậc %d/%d (%.1f rps) | Chờ %d req dở dang... | Settle còn: %.0fs/%.0fs",
					colorYellow, colorReset, stepIdx, totalSteps, rps, curInflight, remainSettle, settleSeconds)
			}
			if statusFilePath != "" {
				_ = os.WriteFile(statusFilePath, []byte(fmt.Sprintf(
					"PHASE=SETTLING\nSTEP=%d/%d\nRPS=%.1f\nINFLIGHT=%d\nREMAIN_SETTLE=%.0fs\nSETTLE_TIMEOUT=%.0fs\nSHOULD_COLLECT=YES\nACTION=DANG_DOI_REQUEST_DO_DANG_HOAN_TAT\n",
					stepIdx, totalSteps, rps, curInflight, remainSettle, settleSeconds)), 0644)
			}
		}
	}

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-doneStatus:
				return
			case now := <-ticker.C:
				updateStatus(now)
			}
		}
	}()

	// Dùng nhãn launchLoop để lệnh break thoát hẳn vòng lặp for khi context bị hủy (ctx.Done())
	// hoặc quá deadline bước test. Trong Go, lệnh break không nhãn bên trong select chỉ thoát khỏi
	// khối select chứ không dừng vòng lặp bao ngoài.
launchLoop:
	for {
		select {
		case <-ctx.Done():
			break launchLoop
		default:
		}

		curN := n.Load()
		targetTime := startTime.Add(time.Duration(float64(curN) * float64(interval)))
		now := time.Now()
		if targetTime.After(stepDeadline) || now.After(stepDeadline) {
			break launchLoop
		}

		delay := targetTime.Sub(now)
		if delay > 0 {
			time.Sleep(delay)
		}
		n.Add(1)

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
			addRecord(rec)
		}()
	}

	// Chuyển sang giai đoạn chờ settle các request in-flight dở dang
	settleStartNano.Store(time.Now().UnixNano())
	phase.Store(1)
	updateStatus(time.Now())

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

	// Dừng goroutine hiển thị trạng thái động và xóa sạch dòng trạng thái
	close(doneStatus)
	if term {
		fmt.Print("\r\033[K")
	}

	// Dừng thu thập record: từ thời điểm này, các goroutine về muộn sẽ bị addRecord() bỏ qua
	// để bảo vệ slice records khỏi race condition khi đọc và ghi file.
	recMu.Lock()
	collecting = false
	launched := n.Load() - dropped.Load()
	abandoned := launched - int64(len(records))

	// Các request đã phát ra nhưng không kịp phản hồi trong thời gian settle được ghi nhận là "abandoned".
	// report.go dùng chuỗi err == "abandoned" làm tín hiệu nhận diện điểm gãy quá tải (knee).
	// Bổ sung các bản ghi còn thiếu giúp số lượng record khớp đúng với tổng request đã gửi.
	for i := int64(0); i < abandoned; i++ {
		records = append(records, scenarios.Record{
			Type:   "req",
			TStart: tStepStart,
			Ok:     false,
			Err:    strPtrMain("abandoned"),
		})
	}
	recMu.Unlock()

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
		Sent:               int(n.Load() - dropped.Load()),
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
		envFile      = flag.String("env-file", ".env", "đường dẫn file .env nạp cấu hình (mặc định: .env)")
		insecure     = flag.Bool("insecure", false, "bỏ qua verify chứng chỉ TLS (khi qua Ingress cert tự ký)")
		cacheBust    = flag.Bool("cache-bust", false, "thêm mã ngẫu nhiên vào câu hỏi để tránh cache")
		stopOnKnee   = flag.Bool("stop-on-knee", false, "dừng khi achieved < 90% offered")
		outFile      = flag.String("out", "loadtest-ramp.jsonl", "file JSONL xuất kết quả kiểm thử tải")
		statusFile   = flag.String("status-file", "/tmp/capsizer-status.txt", "file ghi trạng thái thời gian thực (để tách panel riêng theo dõi hoặc cho script giám sát)")
		baselineWait = flag.Float64("baseline-wait", 90.0, "thời gian chờ đo CPU nền trước khi bắn bậc 1 (giây; 0 để bỏ qua)")
		skipWarmup   = flag.Bool("skip-warmup", false, "bỏ qua request warmup/smoke-test trước khi bắt đầu")
	)

	var headersFlag headerList
	flag.Var(&headersFlag, "H", "thêm HTTP header tùy chọn (ví dụ: -H 'X-Api-Key: 123')")
	flag.Var(&headersFlag, "header", "thêm HTTP header tùy chọn")
	flag.Parse()

	// Nạp biến môi trường từ .env nếu có
	scenarios.LoadDotEnv(*envFile)

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
		queries := scenarios.ResolveQueries(*queriesFile, *envFile)
		if len(queries) == 0 {
			fmt.Fprintln(os.Stderr, "ERROR: không tìm thấy danh sách câu hỏi kiểm thử. Vui lòng cung cấp file câu hỏi qua cờ --queries hoặc khai báo CHAT_QUERIES / CHAT_QUERIES_FILE trong .env")
			os.Exit(2)
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

		// Kiểm tra kích thước pool user (Rate-limit guard):
		// Nếu chu kỳ lặp lại token (cycleS = pool.Len() / maxStep) nhỏ hơn ngưỡng an toàn (120s),
		// cảnh báo nguy cơ chạm rate-limit của từng tài khoản thay vì đo tải thực tế của hệ thống.
		maxStep := 0.0
		for _, s := range steps {
			if s > maxStep {
				maxStep = s
			}
		}
		if maxStep > 0 {
			cycleS := float64(pool.Len()) / maxStep
			if cycleS < 120.0 {
				if pool.Len() == 1 {
					fmt.Fprintf(os.Stderr, "CẢNH BÁO: Đang dùng 1 token duy nhất cho đỉnh tải %.1f req/s. Nguy cơ chạm rate-limit của tài khoản thay vì đo công suất hệ thống.\n", maxStep)
				} else {
					fmt.Fprintf(os.Stderr, "CẢNH BÁO: Pool chỉ có %d user cho đỉnh tải %.1f req/s (chu kỳ lặp lại %.1fs < 120s). Nguy cơ chạm rate-limit của tài khoản thay vì đo công suất hệ thống.\n", pool.Len(), maxStep, cycleS)
				}
			}
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

	term := isTerminal()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		if term {
			fmt.Print("\r\033[K")
		}
		fmt.Println("\nNhận tín hiệu dừng (Ctrl+C). Đang hoàn tất...")
		if *statusFile != "" {
			_ = os.WriteFile(*statusFile, []byte("PHASE=INTERRUPTED\nSHOULD_COLLECT=NO\nACTION=DUNG_TIEN_TRINH_KIEM_THU\n"), 0644)
		}
		cancel()
	}()

	fmt.Printf("target   : %s\n", *targetURL)
	fmt.Printf("scenario : %s\n", scenario.Name())
	fmt.Printf("steps    : %v x %.0fs (settle %.0fs)\n", steps, *stepSeconds, *settle)
	fmt.Printf("out      : %s\n", *outFile)

	// 1. Thực hiện Warmup / Smoke test (1 request dry-run làm ấm kết nối và kiểm tra tính sẵn sàng)
	if !*skipWarmup {
		if err := scenario.Setup(ctx, client); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(2)
		}
	}

	// 2. Chờ đo CPU nền (Idle Baseline) đồng nhất trước khi phát tải
	if *baselineWait > 0 {
		baseStart := time.Now()
		ticker := time.NewTicker(250 * time.Millisecond)
		baseDone := false

		for !baseDone {
			select {
			case <-ctx.Done():
				baseDone = true
			case now := <-ticker.C:
				elapsed := now.Sub(baseStart).Seconds()
				remain := *baselineWait - elapsed
				if remain <= 0 {
					baseDone = true
					break
				}
				if term {
					fmt.Fprintf(os.Stdout, "\r\033[K%s[⏳ ĐO CPU NỀN - BASELINE]%s Để hệ thống nghỉ để probe đo CPU nền... còn %.0fs/%.0fs",
						colorBlue, colorReset, remain, *baselineWait)
				}
				if *statusFile != "" {
					_ = os.WriteFile(*statusFile, []byte(fmt.Sprintf(
						"PHASE=BASELINE\nREMAIN=%.0fs\nTOTAL=%.0fs\nSHOULD_COLLECT=YES\nACTION=DANG_DO_CPU_NEN_IDLE\n",
						remain, *baselineWait)), 0644)
				}
			}
		}
		ticker.Stop()
		if term {
			fmt.Print("\r\033[K")
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		fmt.Printf("baseline : hoàn tất đo CPU nền (%.0fs). Bắt đầu phát tải...\n", *baselineWait)
	}

	p50Lbl := fmt.Sprintf("%s_p50", scenario.MetricLabel())
	p95Lbl := fmt.Sprintf("%s_p95", scenario.MetricLabel())
	hdr := fmt.Sprintf("%8s %9s %6s %5s %5s %9s %9s %9s",
		"offered", "achieved", "ok", "err", "drop", p50Lbl, p95Lbl, "inflight")
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	for i, rps := range steps {
		select {
		case <-ctx.Done():
			break
		default:
		}

		res, err := runStep(ctx, client, scenario, i+1, len(steps), rps, *stepSeconds, *settle, *maxInflight, outWriter, *statusFile, term)
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
			cooldownStart := time.Now()
			cooldownTicker := time.NewTicker(250 * time.Millisecond)
			cooldownDone := false

			for !cooldownDone {
				select {
				case <-ctx.Done():
					cooldownDone = true
				case now := <-cooldownTicker.C:
					elapsed := now.Sub(cooldownStart).Seconds()
					remain := *cooldown - elapsed
					if remain <= 0 {
						cooldownDone = true
						break
					}
					if term {
						fmt.Fprintf(os.Stdout, "\r\033[K%s[☕ NGHỈ COOLDOWN - KHÔNG THU THẬP]%s Nghỉ xả tải giữa 2 bậc... còn %.0fs/%.0fs",
							colorBlue, colorReset, remain, *cooldown)
					}
					if *statusFile != "" {
						_ = os.WriteFile(*statusFile, []byte(fmt.Sprintf(
							"PHASE=COOLDOWN\nSTEP_AFTER=%d/%d\nREMAIN=%.0fs\nTOTAL=%.0fs\nSHOULD_COLLECT=NO\nACTION=HE_THONG_DANG_NGHI_KHONG_THU_THAP\n",
							i+1, len(steps), remain, *cooldown)), 0644)
					}
				}
			}
			cooldownTicker.Stop()
			if term {
				fmt.Print("\r\033[K")
			}
		}
	}

	if *statusFile != "" {
		_ = os.WriteFile(*statusFile, []byte("PHASE=FINISHED\nSHOULD_COLLECT=NO\nACTION=HOAN_TAT_RAMP\n"), 0644)
	}

	_ = outWriter.Flush()

	fmt.Println("\nXong. Ghép với probe rồi dựng bảng:")
	fmt.Printf("  go run report.go --ramp %s --probe <probe.jsonl>\n", *outFile)
}
