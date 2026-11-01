package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type BatchResult struct {
	Scenario   string
	Profile    string
	KneePoint  string
	P95Latency string
	Status     string
	ReportPath string
}

// RunBatch chạy tự động tuần tự nhiều kịch bản và tổng hợp kết quả.
func RunBatch(ctx context.Context, baseCfg *Config, scenarios []string) error {
	if len(scenarios) == 0 {
		// Tự động quét thư mục scenarios/
		entries, err := os.ReadDir("scenarios")
		if err != nil {
			return fmt.Errorf("không đọc được thư mục scenarios: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				scDir := filepath.Join("scenarios", e.Name())
				if fileExists(filepath.Join(scDir, "scenario.json")) || fileExists(filepath.Join(scDir, "scenario.env")) {
					scenarios = append(scenarios, e.Name())
				}
			}
		}
	}

	if len(scenarios) == 0 {
		return fmt.Errorf("không tìm thấy kịch bản nào để chạy batch")
	}

	batchID := fmt.Sprintf("batch_%s", time.Now().Format("20060102_150405"))
	batchDir := filepath.Join("benchmark-results", batchID)
	_ = os.MkdirAll(batchDir, 0755)

	fmt.Println("================================================================================")
	fmt.Printf("    CAPSIZER BATCH RUNNER: CHẠY TỰ ĐỘNG %d KỊCH BẢN (GOLANG)\n", len(scenarios))
	fmt.Println("================================================================================")
	fmt.Printf("Danh sách kịch bản : %v\n", scenarios)
	fmt.Printf("Thư mục Batch Log  : %s\n", batchDir)
	fmt.Println("================================================================================")

	var results []BatchResult

	for idx, sc := range scenarios {
		fmt.Println("\n>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>")
		fmt.Printf("[%d/%d] BẮT ĐẦU CHẠY KỊCH BẢN: %s\n", idx+1, len(scenarios), sc)
		fmt.Println(">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>")

		scOut := filepath.Join(batchDir, sc)
		_ = os.MkdirAll(scOut, 0755)

		// Sao chép cấu hình nền cho kịch bản này
		scCfg := *baseCfg
		scCfg.OutDir = scOut
		if err := scCfg.LoadScenarioPackage(sc); err != nil {
			fmt.Printf("[ERROR] Nạp kịch bản %s thất bại: %v\n", sc, err)
			results = append(results, BatchResult{
				Scenario: sc,
				Status:   "CONFIG_ERR",
			})
			continue
		}

		runner, err := NewRunner(&scCfg)
		if err != nil {
			fmt.Printf("[ERROR] Khởi tạo runner thất bại: %v\n", err)
			results = append(results, BatchResult{
				Scenario: sc,
				Status:   "INIT_ERR",
			})
			continue
		}

		runErr := runner.Run(ctx)
		status := "SUCCESS"
		if runErr != nil {
			status = "FAILED"
		}

		res := BatchResult{
			Scenario:   sc,
			Profile:    scCfg.ProfileType,
			KneePoint:  "None",
			P95Latency: "-",
			Status:     status,
			ReportPath: filepath.Join(scOut, "phase1-report.txt"),
		}

		// Đọc kết quả từ file report nếu có
		if fileExists(res.ReportPath) {
			data, err := os.ReadFile(res.ReportPath)
			if err == nil {
				text := string(data)
				if stringsContains(text, "Điểm gãy quan sát được") {
					res.KneePoint = "Phát hiện Knee"
				} else {
					res.KneePoint = "Khỏe (No Knee)"
				}
			}
		}

		results = append(results, res)
	}

	// In bảng tổng hợp
	summaryFile := filepath.Join(batchDir, "batch_summary.txt")
	f, _ := os.Create(summaryFile)
	defer f.Close()

	printHeader := func() {
		header := fmt.Sprintf("%-18s | %-12s | %-15s | %-12s | %-12s | %-10s\n",
			"SCENARIO", "PROFILE", "KNEE POINT", "P95 LATENCY", "STATUS", "REPORT LINK")
		fmt.Println(header)
		if f != nil {
			_, _ = f.WriteString(header)
		}
	}

	fmt.Println("\n================================================================================")
	fmt.Println("          BẢNG TỔNG HỢP SO SÁNH NĂNG LỰC CÁC DỊCH VỤ (BATCH RUN)")
	fmt.Println("================================================================================")
	printHeader()

	for _, r := range results {
		row := fmt.Sprintf("%-18s | %-12s | %-15s | %-12s | %-12s | %-10s\n",
			r.Scenario, r.Profile, r.KneePoint, r.P95Latency, r.Status, r.ReportPath)
		fmt.Print(row)
		if f != nil {
			_, _ = f.WriteString(row)
		}
	}

	fmt.Println("\n================================================================================")
	fmt.Println("✓ HOÀN THÀNH TOÀN BỘ PHIÊN CHẠY BATCH!")
	fmt.Printf("Báo cáo tổng hợp: %s\n", summaryFile)
	fmt.Println("================================================================================")

	return nil
}

func stringsContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && containsHelper(s, substr)))
}

func containsHelper(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
