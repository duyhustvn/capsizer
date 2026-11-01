package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"capsizer/pkg/analyzer"
)

// Runner thực thi quy trình chuẩn 2 Pha định chuẩn năng lực & profiling.
type Runner struct {
	cfg       *Config
	loadSSH   *SSHClient
	k8sSSH    *SSHClient
	k8sClient *K8sClient
}

// NewRunner khởi tạo Runner từ Config.
func NewRunner(cfg *Config) (*Runner, error) {
	r := &Runner{cfg: cfg}

	// 1. Khởi tạo SSH tới Máy Phát Tải (Machine 2) nếu mode=remote
	if cfg.LoadgenMode == "remote" {
		if cfg.LoadgenHost == "" {
			return nil, fmt.Errorf("chế độ phát tải remote yêu cầu cấu hình LoadgenHost hoặc REMOTE_HOST")
		}
		loadSSH, err := NewSSHClient(cfg.LoadgenUser, cfg.LoadgenHost, cfg.LoadgenPort, cfg.LoadgenKey)
		if err != nil {
			return nil, fmt.Errorf("lỗi cấu hình SSH máy phát tải: %w", err)
		}
		r.loadSSH = loadSSH
	}

	// 2. Khởi tạo SSH tới Server K8s (Machine 3) nếu có k8s-ssh-host
	if cfg.K8sSSHHost != "" {
		k8sSSH, err := NewSSHClient(cfg.K8sSSHUser, cfg.K8sSSHHost, cfg.K8sSSHPort, cfg.K8sSSHKey)
		if err != nil {
			return nil, fmt.Errorf("lỗi cấu hình SSH cụm K8s: %w", err)
		}
		r.k8sSSH = k8sSSH
	}

	// 3. Khởi tạo K8sClient
	r.k8sClient = NewK8sClient(r.k8sSSH, cfg.K8sUseSudo, cfg.K8sNamespace, cfg.KubeconfigPath, cfg.ContextName)

	return r, nil
}

// Close giải phóng các kết nối SSH.
func (r *Runner) Close() {
	if r.loadSSH != nil {
		_ = r.loadSSH.Close()
	}
	if r.k8sSSH != nil {
		_ = r.k8sSSH.Close()
	}
}

// PreflightCheck kiểm tra toàn diện liên kết 3 máy.
func (r *Runner) PreflightCheck(ctx context.Context) error {
	fmt.Println("================================================================================")
	fmt.Println("         KIỂM TRA LIÊN KẾT 3 MÁY (PREFLIGHT HEALTH CHECK - GO RUNNER)")
	fmt.Println("================================================================================")

	// [1/3] Máy chạy Skill
	fmt.Println("[1/3] Máy chạy Skill (Orchestrator - Local):")
	goVer, err := exec.Command("go", "version").Output()
	if err == nil {
		fmt.Printf("      ✓ Golang: %s", string(goVer))
	}
	fmt.Println("      ✓ Trạng thái: Sẵn sàng điều phối")

	// [2/3] Máy phát tải
	fmt.Println("\n[2/3] Máy phát tải (Load Generator Host):")
	if r.cfg.LoadgenMode == "remote" {
		out, err := r.loadSSH.RunCommand(ctx, "uname -srm")
		if err != nil {
			return fmt.Errorf("không thể kết nối SSH tới máy phát tải: %w", err)
		}
		fmt.Printf("      ✓ Kết nối SSH thành công (%s@%s:%d) - OS: %s\n",
			r.cfg.LoadgenUser, r.cfg.LoadgenHost, r.cfg.LoadgenPort, out)
	} else {
		fmt.Println("      ✓ Chế độ phát tải cục bộ (Local Host).")
	}

	// [3/3] Cụm K8s
	fmt.Println("\n[3/3] Cụm Kubernetes từ xa (Remote K8s Target):")
	if r.k8sSSH != nil {
		out, err := r.k8sSSH.RunCommand(ctx, "uname -srm")
		if err != nil {
			return fmt.Errorf("không thể kết nối SSH tới server cụm K8s: %w", err)
		}
		fmt.Printf("      ✓ Kết nối SSH server K8s thành công (%s@%s:%d) - OS: %s\n",
			r.cfg.K8sSSHUser, r.cfg.K8sSSHHost, r.cfg.K8sSSHPort, out)
	}

	info, err := r.k8sClient.ClusterInfo(ctx)
	if err != nil {
		return fmt.Errorf("không thể kết nối K8s API Server qua kubectl: %w", err)
	}
	infoFirst := strings.Split(info, "\n")[0]
	fmt.Printf("      ✓ K8s API Server: %s\n", infoFirst)

	// Kiểm tra Pod mục tiêu
	if r.cfg.K8sSelector != "" || r.cfg.K8sPod != "" {
		pod, err := r.k8sClient.ResolveTargetPod(ctx, r.cfg.K8sSelector, r.cfg.K8sPod)
		if err != nil {
			return fmt.Errorf("lỗi tìm Pod mục tiêu: %w", err)
		}
		r.cfg.K8sPod = pod
		fmt.Printf("      ✓ Pod mục tiêu: %s/%s\n", r.cfg.K8sNamespace, pod)

		if r.cfg.SetPodCPU != "" {
			wType, wName, err := r.k8sClient.DetectTargetWorkload(ctx, pod, r.cfg.K8sSelector)
			if err != nil {
				fmt.Printf("      [CẢNH BÁO] Không thể nhận diện Workload cha để điều chỉnh CPU: %v\n", err)
			} else {
				fmt.Printf("      ✓ Workload cha: %s/%s (sẵn sàng điều chỉnh CPU: %s core)\n", wType, wName, r.cfg.SetPodCPU)
			}
		}
	}

	fmt.Println("================================================================================")
	fmt.Println("✓ TẤT CẢ LIÊN KẾT 3 MÁY ĐỀU SẴN SÀNG!")
	fmt.Println("================================================================================")
	return nil
}

// Run thực thi toàn bộ quy trình 2 Pha.
func (r *Runner) Run(ctx context.Context) error {
	defer r.Close()

	if r.cfg.OnlyCheck {
		return r.PreflightCheck(ctx)
	}

	if r.cfg.OutDir == "" {
		r.cfg.OutDir = filepath.Join("benchmark-results", time.Now().Format("20060102_150405"))
	}
	_ = os.MkdirAll(r.cfg.OutDir, 0755)

	// In banner
	r.printBanner()

	// 1. Tự động tìm Pod nếu chưa có
	if r.cfg.K8sPod == "" && r.cfg.K8sSelector != "" {
		pod, err := r.k8sClient.ResolveTargetPod(ctx, r.cfg.K8sSelector, "")
		if err != nil {
			return err
		}
		r.cfg.K8sPod = pod
	}

	// 2. Điều chỉnh CPU Pod nếu có yêu cầu (Scale to 1 Core) & Setup Defer Restore
	if r.cfg.SetPodCPU != "" && r.cfg.K8sPod != "" {
		cName, _ := r.k8sClient.GetContainerName(ctx, r.cfg.K8sPod, r.cfg.K8sContainer)
		r.cfg.K8sContainer = cName

		wType, wName, err := r.k8sClient.DetectTargetWorkload(ctx, r.cfg.K8sPod, r.cfg.K8sSelector)
		if err == nil {
			fmt.Printf("==> [Cấu hình CPU] Đang đưa %s/%s về %s core...\n", wType, wName, r.cfg.SetPodCPU)
			origJSON, cIdx, err := r.k8sClient.ApplyWorkloadCPU(ctx, wType, wName, cName, r.cfg.SetPodCPU)
			if err != nil {
				fmt.Printf("    [CẢNH BÁO] Không thể điều chỉnh CPU: %v\n", err)
			} else {
				fmt.Printf("    ✓ Đã cấu hình CPU %s core (Bảo lưu: %s)\n", r.cfg.SetPodCPU, origJSON)

				// Defer hoàn nguyên CPU khi thoát
				if r.cfg.RestorePodCPU {
					defer func() {
						fmt.Println("\n==> [Khôi phục Hạ tầng] Đang hoàn nguyên cấu hình CPU ban đầu...")
						_ = r.k8sClient.RestoreWorkloadCPU(context.Background(), wType, wName, cIdx, origJSON)
						fmt.Println("    ✓ Đã hoàn nguyên thành công.")
					}()
				}

				// Chờ Pod mới sẵn sàng
				fmt.Println("    • Đang chờ Pod mới Ready sau rollout...")
				newPod, err := r.k8sClient.WaitForPodReady(ctx, r.cfg.K8sSelector, 180*time.Second)
				if err == nil {
					r.cfg.K8sPod = newPod
					fmt.Printf("    ✓ Pod mới đã sẵn sàng: %s\n", newPod)
				}
			}
		}
	}

	// 3. Đồng bộ máy phát tải từ xa nếu mode=remote
	if r.cfg.LoadgenMode == "remote" {
		fmt.Println("==> [Máy phát tải] Đồng bộ binary và kịch bản lên máy bắn tải...")
		if err := r.setupRemoteLoadgen(ctx); err != nil {
			return err
		}
	}

	// 4. [PHA 1] Đo Capacity Sizing sạch (Ramp + Probe)
	fmt.Println("\n▶ [PHA 1] KHỞI ĐỘNG ĐO CAPACITY SIZING SẠCH (KHÔNG BẬT PROFILER)...")

	// Khởi chạy probe trong Pod
	if r.cfg.K8sPod != "" {
		fmt.Println("==> Đang khởi chạy probe trong Pod mục tiêu...")
		probeCmd := fmt.Sprintf("nohup probe -port %d -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &", r.cfg.TargetPort)
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, probeCmd)

		defer func() {
			fmt.Println("==> Dừng probe trong Pod...")
			_, _ = r.k8sClient.ExecInPod(context.Background(), r.cfg.K8sPod, r.cfg.K8sContainer, "pkill -f 'probe' || true")
		}()
	}

	time.Sleep(2 * time.Second)

	// Chạy Ramp Pha 1
	rampLog := filepath.Join(r.cfg.OutDir, "phase1-ramp.jsonl")
	probeLog := filepath.Join(r.cfg.OutDir, "phase1-probe.jsonl")
	reportLog := filepath.Join(r.cfg.OutDir, "phase1-report.txt")

	fmt.Printf("==> Bắt đầu phát tải Open-Loop (đếm lùi %ds lấy mẫu nền)...\n", r.cfg.BaselineWait)
	if err := r.runLoadgen(ctx, r.cfg.Steps, r.cfg.StepSeconds, r.cfg.BaselineWait, r.cfg.Cooldown, rampLog, false); err != nil {
		return fmt.Errorf("lỗi phát tải Pha 1: %w", err)
	}
	fmt.Println("==> Hoàn thành phát tải Pha 1!")

	// Thu thập file probe.jsonl
	if r.cfg.K8sPod != "" {
		fmt.Println("==> Đang tải file probe.jsonl về...")
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, "pkill -f 'probe' || true")
		time.Sleep(1 * time.Second)
		_ = r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, "/tmp/probe.jsonl", probeLog)
	}

	// Phân tích kết quả Pha 1 với pkg/analyzer
	fmt.Println("\n==> Đang phân tích kết quả định chuẩn Pha 1...")
	rampRecords, err := analyzer.LoadRamp(rampLog)
	if err != nil {
		return fmt.Errorf("không đọc được file ramp: %w", err)
	}

	var probeRecords []analyzer.ProbeRecord
	if fileExists(probeLog) {
		probeRecords, _ = analyzer.LoadProbe(probeLog)
	}

	report, err := analyzer.Analyze(rampRecords, probeRecords)
	if err != nil {
		return fmt.Errorf("phân tích sizing thất bại: %w", err)
	}

	// Ghi file report
	_ = os.WriteFile(reportLog, []byte(report.ReportText), 0644)
	fmt.Print(report.ReportText)

	if !report.HasKnee || report.KneeRPS <= 0 {
		fmt.Println("\n================================================================================")
		fmt.Println("✓ KẾT QUẢ PHA 1: HỆ THỐNG HOẠT ĐỘNG HOÀN TOÀN KHỎE MẠNH TRONG DẢI BẬC ĐÃ TEST!")
		fmt.Println("  Không phát hiện điểm gãy (Knee Point). Bạn có thể nâng dải --steps lên để tìm trần.")
		fmt.Println("================================================================================")
		return nil
	}

	// 5. [PHA 2] Chẩn đoán sâu tại Knee Point
	fmt.Println("\n================================================================================")
	fmt.Printf("⚠️ PHÁT HIỆN ĐIỂM GÃY TẠI MỨC TẢI: %.1f req/s\n", report.KneeRPS)
	fmt.Println("▶ [PHA 2] TỰ ĐỘNG CHUYỂN SANG CHẨN ĐOÁN NGUYÊN NHÂN SÂU (FLAMEGRAPH)...")
	fmt.Println("================================================================================")

	profilers := r.parseProfilers(r.cfg.ProfileType)
	profDuration := 30
	totalDuration := len(profilers)*(profDuration+5) + 10

	phase2RampLog := filepath.Join(r.cfg.OutDir, "phase2-ramp.jsonl")

	// Bắn tải duy trì ở background
	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	go func() {
		kneeStr := fmt.Sprintf("%.1f", report.KneeRPS)
		_ = r.runLoadgen(bgCtx, kneeStr, totalDuration, 0, 0, phase2RampLog, true)
	}()

	time.Sleep(5 * time.Second)

	generatedSVGs := []string{}
	for _, prof := range profilers {
		fmt.Printf("\n--> [PROFILING: %s] Đang lấy mẫu %ds...\n", prof, profDuration)
		localSVG := filepath.Join(r.cfg.OutDir, fmt.Sprintf("flamegraph_knee_%.0frps_%s.svg", report.KneeRPS, prof))

		if r.cfg.K8sPod != "" {
			if prof == "pyspy" {
				pyspyCmd := fmt.Sprintf("py-spy record -o /tmp/flamegraph_%s.svg -d %d --rate 100 --subprocesses || true", prof, profDuration)
				_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, pyspyCmd)
				_ = r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, fmt.Sprintf("/tmp/flamegraph_%s.svg", prof), localSVG)
			} else if prof == "perf" {
				perfCmd := fmt.Sprintf("profile-perf %d /tmp/flamegraph_%s.svg || true", profDuration, prof)
				_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, perfCmd)
				_ = r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, fmt.Sprintf("/tmp/flamegraph_%s.svg", prof), localSVG)
			}
		}

		if fileExists(localSVG) {
			generatedSVGs = append(generatedSVGs, localSVG)
			fmt.Printf("    ✓ Đã xuất FlameGraph (%s): %s\n", prof, localSVG)
		}
		time.Sleep(3 * time.Second)
	}

	bgCancel()

	fmt.Println("\n================================================================================")
	fmt.Println("✓ HOÀN TẤT TOÀN DIỆN QUY TRÌNH 2 PHA ĐỊNH CHUẨN VÀ PROFILING!")
	fmt.Println("================================================================================")
	fmt.Printf("Báo cáo phân tích Sizing : %s\n", reportLog)
	for _, s := range generatedSVGs {
		fmt.Printf("  • FlameGraph SVG: %s\n", s)
	}
	fmt.Println("================================================================================")
	return nil
}

func (r *Runner) runLoadgen(ctx context.Context, steps string, stepSec, baseWait, cooldown int, outPath string, background bool) error {
	if r.cfg.LoadgenMode == "remote" {
		// Chạy qua SSH trên Machine 2
		remoteRamp := fmt.Sprintf("%s/bin/capsizer-ramp", r.cfg.LoadgenDir)
		remoteLog := fmt.Sprintf("%s/current-ramp.jsonl", r.cfg.LoadgenDir)
		remoteScen := fmt.Sprintf("%s/scenarios/%s", r.cfg.LoadgenDir, r.cfg.Scenario)

		cmd := fmt.Sprintf("%s -scenario %s -steps %s -step-seconds %d -baseline-wait %d -cooldown %d -out %s",
			remoteRamp, remoteScen, steps, stepSec, baseWait, cooldown, remoteLog)

		if r.cfg.URL != "" {
			cmd += fmt.Sprintf(" -url %s", r.cfg.URL)
		}

		if background {
			_, err := r.loadSSH.RunCommand(ctx, cmd)
			return err
		}

		_, err := r.loadSSH.RunCommand(ctx, cmd)
		if err == nil {
			_ = r.loadSSH.DownloadFile(ctx, remoteLog, outPath)
		}
		return err
	}

	// Chạy cục bộ bằng capsizer-ramp
	args := []string{
		"-steps", steps,
		"-step-seconds", fmt.Sprintf("%d", stepSec),
		"-baseline-wait", fmt.Sprintf("%d", baseWait),
		"-cooldown", fmt.Sprintf("%d", cooldown),
		"-out", outPath,
	}
	if r.cfg.ScenarioDir != "" {
		args = append(args, "-scenario", r.cfg.ScenarioDir)
	} else if r.cfg.Scenario != "" {
		args = append(args, "-scenario", r.cfg.Scenario)
	}
	if r.cfg.URL != "" {
		args = append(args, "-url", r.cfg.URL)
	}

	cmd := exec.CommandContext(ctx, "capsizer-ramp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *Runner) setupRemoteLoadgen(ctx context.Context) error {
	remoteDir := r.cfg.LoadgenDir
	_, _ = r.loadSSH.RunCommand(ctx, fmt.Sprintf("mkdir -p %s/bin %s/scenarios/%s", remoteDir, remoteDir, r.cfg.Scenario))

	// Đồng bộ kịch bản
	if r.cfg.ScenarioDir != "" {
		entries, err := os.ReadDir(r.cfg.ScenarioDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					localF := filepath.Join(r.cfg.ScenarioDir, e.Name())
					remoteF := fmt.Sprintf("%s/scenarios/%s/%s", remoteDir, r.cfg.Scenario, e.Name())
					_ = r.loadSSH.UploadFile(ctx, localF, remoteF)
				}
			}
		}
	}
	return nil
}

func (r *Runner) parseProfilers(raw string) []string {
	parts := strings.Split(strings.ReplaceAll(raw, " ", ","), ",")
	var res []string
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "all" {
			res = append(res, "pyspy", "perf")
		} else if p != "" {
			res = append(res, p)
		}
	}
	if len(res) == 0 {
		res = append(res, "pyspy")
	}
	return res
}

func (r *Runner) printBanner() {
	fmt.Println("================================================================================")
	fmt.Println("    CAPSIZER 2-PHASE CAPACITY SIZING & PROFILING (GOLANG RUNNER)")
	fmt.Println("================================================================================")
	fmt.Println("[1] Máy chạy Skill    : Local Host (Go Orchestrator)")
	if r.cfg.LoadgenMode == "remote" {
		fmt.Printf("[2] Máy phát tải      : REMOTE (Host: %s@%s:%d)\n", r.cfg.LoadgenUser, r.cfg.LoadgenHost, r.cfg.LoadgenPort)
	} else {
		fmt.Println("[2] Máy phát tải      : LOCAL (Chính máy này)")
	}
	if r.cfg.K8sSSHHost != "" {
		fmt.Printf("[3] Server cụm K8s    : SSH %s@%s:%d -> Pod %s/%s\n",
			r.cfg.K8sSSHUser, r.cfg.K8sSSHHost, r.cfg.K8sSSHPort, r.cfg.K8sNamespace, r.cfg.K8sPod)
	} else if r.cfg.K8sPod != "" {
		fmt.Printf("[3] Cụm K8s mục tiêu  : Local Kubectl -> Pod %s/%s\n", r.cfg.K8sNamespace, r.cfg.K8sPod)
	}
	fmt.Printf("Endpoint nhận request : %s (Scenario: %s)\n", r.cfg.URL, r.cfg.Scenario)
	fmt.Printf("Dải bậc tải           : %s (Mỗi bậc: %ds, Cooldown: %ds)\n", r.cfg.Steps, r.cfg.StepSeconds, r.cfg.Cooldown)
	if r.cfg.SetPodCPU != "" {
		fmt.Printf("Cấu hình CPU Pod      : Tự động đưa về %s core (Khôi phục khi xong: %v)\n", r.cfg.SetPodCPU, r.cfg.RestorePodCPU)
	}
	fmt.Printf("Thư mục output        : %s\n", r.cfg.OutDir)
	fmt.Println("================================================================================")
}
