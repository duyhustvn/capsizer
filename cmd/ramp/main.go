package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"capsizer/pkg/engine"
	"capsizer/pkg/scenarios"
)

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

func fmtFloatPtr(v *float64, nd int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.*f", nd, *v)
}

// JSONLWriter triển khai engine.RecordWriter.
type JSONLWriter struct {
	writer *bufio.Writer
}

func (w *JSONLWriter) WriteRecord(record any) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = w.writer.Write(append(line, '\n'))
	return err
}

func (w *JSONLWriter) Flush() error {
	return w.writer.Flush()
}

type headerList []string

func (h *headerList) String() string {
	return strings.Join(*h, ", ")
}

func (h *headerList) Set(val string) error {
	*h = append(*h, val)
	return nil
}

// kvMap hỗ trợ truyền danh sách key=value tùy biến cho Scenario.
type kvMap map[string]string

func (m *kvMap) String() string {
	var pairs []string
	for k, v := range *m {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(pairs, ", ")
}

func (m *kvMap) Set(val string) error {
	parts := strings.SplitN(val, "=", 2)
	if len(parts) == 2 {
		(*m)[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	} else if len(parts) == 1 {
		(*m)[strings.TrimSpace(parts[0])] = "true"
	}
	return nil
}

func main() {
	var (
		// ---------------------------------------------------------------------
		// CÁC THAM SỐ CỐT LÕI (CORE RAMP ENGINE)
		// ---------------------------------------------------------------------
		scenarioName = flag.String("scenario", "chat-sse", "loại kịch bản: 'chat-sse', 'sample-get', 'sample-post', hoặc kịch bản dịch vụ tùy chỉnh")
		targetURL    = flag.String("url", "http://127.0.0.1:8000/chat", "URL endpoint nhận request")
		stepsStr     = flag.String("steps", "1,2,4,8,16,32", "danh sách RPS, cách nhau dấu phẩy")
		stepSeconds  = flag.Float64("step-seconds", 60.0, "độ dài mỗi bậc (giây)")
		cooldown     = flag.Float64("cooldown", 20.0, "nghỉ giải tỏa tải giữa hai bậc (giây)")
		settle       = flag.Float64("settle", 60.0, "chờ tối đa cho in-flight dở dang cuối bậc (giây)")
		baselineWait = flag.Float64("baseline-wait", 15.0, "chờ đo baseline CPU/RAM nền tĩnh trước khi phát tải (giây)")
		timeoutSec   = flag.Float64("timeout", 180.0, "timeout cho mỗi request (giây)")
		maxInflight  = flag.Int("max-inflight", 5000, "trần concurrency tối đa của client")
		workers      = flag.Int("workers", 2000, "kích thước connection pool (MaxIdleConns)")
		outFile      = flag.String("out", "loadtest-ramp.jsonl", "đường dẫn file JSONL xuất kết quả")
		skipVerify   = flag.Bool("skip-verify", false, "bỏ qua xác thực chứng chỉ TLS/SSL")
		statusFile   = flag.String("status-file", "", "file ghi trạng thái tiến trình thời gian thực")
		envFile      = flag.String("env-file", ".env", "đường dẫn file .env nạp cấu hình môi trường")

		// ---------------------------------------------------------------------
		// THAM SỐ TƯƠNG THÍCH NGƯỢC (LEGACY / CONVENIENCE ALIASES)
		// ---------------------------------------------------------------------
		methodLegacy      = flag.String("method", "", "[Legacy alias] HTTP method cho kịch bản REST")
		bodyLegacy        = flag.String("body", "", "[Legacy alias] chuỗi body gửi kèm cho request")
		bodyFileLegacy    = flag.String("body-file", "", "[Legacy alias] file chứa body payload")
		cacheBustLegacy   = flag.Bool("cache-bust", false, "[Legacy alias] thêm tham số timestamp ngẫu nhiên chống cache")
	)

	var headers headerList
	flag.Var(&headers, "H", "HTTP header tùy chỉnh ('Key: Value'). Có thể dùng nhiều lần.")
	flag.Var(&headers, "header", "HTTP header tùy chỉnh (bí danh của -H)")

	// Tham số động truyền tự do cho bất kỳ kịch bản nào
	scenarioOptions := make(kvMap)
	flag.Var(&scenarioOptions, "opt", "Tùy chọn riêng cho kịch bản dạng 'key=value' (ví dụ: -opt queries=q.txt -opt users=u.csv -opt method=POST)")
	flag.Var(&scenarioOptions, "set", "Bí danh của -opt")
	flag.Var(&scenarioOptions, "param", "Bí danh của -opt")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Sử dụng: capsizer-ramp [OPTIONS]\n\n")
		fmt.Fprintf(os.Stderr, "CÁC THAM SỐ CỐT LÕI (RAMP ENGINE):\n")
		fmt.Fprintf(os.Stderr, "  -url string             URL endpoint nhận request (default \"http://127.0.0.1:8000/chat\")\n")
		fmt.Fprintf(os.Stderr, "  -scenario string        Tên kịch bản (chat-sse, sample-get, sample-post) HOẶC đường dẫn file/thư mục scenario.json (default \"chat-sse\")\n")
		fmt.Fprintf(os.Stderr, "  -steps string           Dải bậc RPS, phân tách bằng dấu phẩy (default \"1,2,4,8,16,32\")\n")
		fmt.Fprintf(os.Stderr, "  -step-seconds float     Thời gian phát tải mỗi bậc (giây, default 60)\n")
		fmt.Fprintf(os.Stderr, "  -baseline-wait float    Thời gian chờ đo baseline CPU/RAM tĩnh trước bậc 1 (giây, default 15)\n")
		fmt.Fprintf(os.Stderr, "  -cooldown float         Thời gian nghỉ giải tỏa tải giữa hai bậc (giây, default 20)\n")
		fmt.Fprintf(os.Stderr, "  -settle float           Thời gian chờ tối đa cho in-flight cuối bậc (giây, default 60)\n")
		fmt.Fprintf(os.Stderr, "  -timeout float          Timeout cho mỗi request (giây, default 180)\n")
		fmt.Fprintf(os.Stderr, "  -max-inflight int       Trần kết nối đồng thời tối đa của client (default 5000)\n")
		fmt.Fprintf(os.Stderr, "  -workers int            Kích thước connection pool MaxIdleConns (default 2000)\n")
		fmt.Fprintf(os.Stderr, "  -out string             File JSONL xuất kết quả đo (default \"loadtest-ramp.jsonl\")\n")
		fmt.Fprintf(os.Stderr, "  -skip-verify            Bỏ qua xác thực chứng chỉ TLS/SSL\n")
		fmt.Fprintf(os.Stderr, "  -status-file string     File ghi trạng thái thời gian thực\n")
		fmt.Fprintf(os.Stderr, "  -H, -header value       HTTP header tùy chỉnh ('Key: Value', dùng nhiều lần)\n")
		fmt.Fprintf(os.Stderr, "  -env-file string        Đường dẫn file .env nạp cấu hình (default \".env\")\n\n")

		fmt.Fprintf(os.Stderr, "TÙY CHỌN RIÊNG CHO TỪNG KỊCH BẢN (-opt key=value):\n")
		fmt.Fprintf(os.Stderr, "  -opt, -set, -param      Truyền tham số tùy biến cho kịch bản (dùng nhiều lần).\n")
		fmt.Fprintf(os.Stderr, "                          Ví dụ nạp hoặc ghi đè Dataset:\n")
		fmt.Fprintf(os.Stderr, "                            -opt dataset.prompts.file=queries.txt -opt dataset.users.file=users.csv\n")
		fmt.Fprintf(os.Stderr, "                          Ví dụ cho REST API (sample-post, sample-get):\n")
		fmt.Fprintf(os.Stderr, "                            -opt method=POST -opt body='{\"msg\":\"hi\"}' -opt body-file=req.json\n\n")

		fmt.Fprintf(os.Stderr, "KỊCH BẢN TÍCH HỢP: %v (hoặc truyền trực tiếp file/thư mục chứa scenario.json tương tự k6)\n", scenarios.AvailableScenarios())
	}

	flag.Parse()

	// Parse header map
	headerMap := make(map[string]string)
	for _, h := range headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			headerMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	// Parse steps
	var steps []float64
	for _, s := range strings.Split(*stepsStr, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		val, err := strconv.ParseFloat(s, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Lỗi cú pháp tham số -steps: %v\n", err)
			os.Exit(1)
		}
		steps = append(steps, val)
	}
	if len(steps) == 0 {
		fmt.Fprintf(os.Stderr, "Lỗi: Danh sách -steps không hợp lệ\n")
		os.Exit(1)
	}

	// Nạp file .env nếu có
	if *envFile != "" {
		scenarios.LoadDotEnv(*envFile)
		scenarioOptions["env-file"] = *envFile
	}

	// Tự động map các cờ legacy vào scenarioOptions nếu người dùng truyền
	if *methodLegacy != "" {
		scenarioOptions["method"] = *methodLegacy
	}
	if *bodyLegacy != "" {
		scenarioOptions["body"] = *bodyLegacy
	}
	if *bodyFileLegacy != "" {
		scenarioOptions["body-file"] = *bodyFileLegacy
	}
	if *cacheBustLegacy {
		scenarioOptions["cache-bust"] = "true"
	}

	methodVal := scenarioOptions["method"]
	if methodVal == "" {
		methodVal = "GET"
	}

	// Khởi tạo Scenario từ Registry với Dynamic Options
	scenarioParams := scenarios.ScenarioParams{
		TargetURL: *targetURL,
		Method:    methodVal,
		Headers:   headerMap,
		Timeout:   time.Duration(*timeoutSec * float64(time.Second)),
		Options:   scenarioOptions,
	}

	sc, err := scenarios.Get(*scenarioName, scenarioParams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi kịch bản: %v\n", err)
		os.Exit(1)
	}

	// Cấu hình Connection Pool & HTTP Client
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          *workers,
		MaxIdleConnsPerHost:   *workers,
		MaxConnsPerHost:       *workers,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: *skipVerify,
		},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(*timeoutSec * float64(time.Second)),
	}

	// Chuẩn bị file output
	if err := os.MkdirAll(filepath.Dir(*outFile), 0755); err != nil && filepath.Dir(*outFile) != "." {
		fmt.Fprintf(os.Stderr, "Lỗi tạo thư mục %s: %v\n", filepath.Dir(*outFile), err)
		os.Exit(1)
	}
	f, err := os.OpenFile(*outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi mở file %s: %v\n", *outFile, err)
		os.Exit(1)
	}
	defer f.Close()

	writer := &JSONLWriter{writer: bufio.NewWriterSize(f, 64*1024)}

	// Setup context & signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nĐang nhận tín hiệu dừng, ngắt kết nối an toàn...")
		cancel()
	}()

	term := isTerminal()

	displayTarget := *targetURL
	if tp, ok := sc.(interface{ Target() string }); ok {
		if (*targetURL == "http://127.0.0.1:8000/chat" || *targetURL == "") && tp.Target() != "" {
			displayTarget = tp.Target()
		}
	}

	fmt.Printf("%s[Capsizer Open-Loop Load Generator]%s Kịch bản: %s%s%s -> %s\n",
		colorBlue, colorReset, colorGreen, sc.Name(), colorReset, displayTarget)
	fmt.Printf("Dải bậc RPS: %v | Mỗi bậc: %.0fs | Cooldown: %.0fs | Settle: %.0fs | File output: %s\n",
		steps, *stepSeconds, *cooldown, *settle, *outFile)

	cfg := engine.EngineConfig{
		Scenario:       sc,
		HTTPClient:     httpClient,
		Steps:          steps,
		StepSeconds:    *stepSeconds,
		Cooldown:       *cooldown,
		SettleSeconds:  *settle,
		BaselineWait:   *baselineWait,
		MaxInflight:    *maxInflight,
		StatusFilePath: *statusFile,
		OnStatus: func(st engine.StatusUpdate) {
			if !term {
				return
			}
			if st.Phase == 0 {
				fmt.Fprintf(os.Stdout, "\r\033[K%s[▶ BẮN TẢI]%s Bậc %d/%d (%.1f rps) | Đã chạy: %.0fs/%.0fs (còn %.0fs) | Inflight: %d | Đã phát: %d",
					colorGreen, colorReset, st.StepIndex, st.TotalSteps, st.RPS, st.Elapsed, st.Duration, st.Remain, st.Inflight, st.Sent)
			} else {
				fmt.Fprintf(os.Stdout, "\r\033[K%s[⏳ CHỜ SETTLE]%s Bậc %d/%d (%.1f rps) | Chờ %d req dở dang... | Settle còn: %.0fs/%.0fs",
					colorYellow, colorReset, st.StepIndex, st.TotalSteps, st.RPS, st.Inflight, st.RemainSettle, st.SettleTimeout)
			}
		},
		OnStepComplete: func(r engine.StepResult) {
			if term {
				fmt.Print("\r\033[K")
			}
			fmt.Printf("✓ Hoàn tất bậc %d/%d (%4.1f rps): Đạt %4.1f req/s | OK: %d | Lỗi: %d | p50: %s ms | p95: %s ms | Inflight cao nhất: %d\n",
				r.StepIndex, r.TotalSteps, r.RpsOffered, r.RpsAchieved, r.CompletedOk, r.Errors,
				fmtFloatPtr(r.P50, 1), fmtFloatPtr(r.P95, 1), r.MaxInflight)
		},
	}

	eng := engine.NewEngine(cfg, writer)
	results, err := eng.Run(ctx)
	if err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "Lỗi trong quá trình chạy: %v\n", err)
	}

	// In bảng tổng kết cuối đợt
	if len(results) > 0 {
		fmt.Println("\n==========================================================================================")
		fmt.Printf("                       TỔNG KẾT BẮN TẢI (SCENARIO: %s)\n", strings.ToUpper(sc.Name()))
		fmt.Println("==========================================================================================")
		fmt.Printf("%-6s | %-10s | %-12s | %-8s | %-8s | %-10s | %-10s | %-10s\n",
			"BẬC", "OFFERED", "ACHIEVED", "OK", "ERRORS", "P50 (MS)", "P95 (MS)", "MAX INFLIGHT")
		fmt.Println("------------------------------------------------------------------------------------------")
		for _, r := range results {
			fmt.Printf("%-6d | %-10.1f | %-12.1f | %-8d | %-8d | %-10s | %-10s | %-10d\n",
				r.StepIndex, r.RpsOffered, r.RpsAchieved, r.CompletedOk, r.Errors,
				fmtFloatPtr(r.P50, 1), fmtFloatPtr(r.P95, 1), r.MaxInflight)
		}
		fmt.Println("==========================================================================================")
		fmt.Printf("Dữ liệu chi tiết từng request đã được ghi vào file: %s\n", *outFile)
	}
}
