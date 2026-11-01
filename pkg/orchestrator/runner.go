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
	cfg        *Config
	loadSSH    *SSHClient
	k8sSSH     *SSHClient
	k8sClient  *K8sClient
	LastReport *analyzer.AnalysisReport
}

// NewRunner khởi tạo Runner từ Config.
func NewRunner(cfg *Config) (*Runner, error) {
	cfg.Finalize()
	r := &Runner{cfg: cfg}

	// 1. Khởi tạo SSH tới Máy Phát Tải (Machine 2) nếu mode=remote
	if cfg.LoadgenMode == "remote" {
		if cfg.LoadgenHost == "" {
			return nil, fmt.Errorf("chế độ phát tải remote yêu cầu cấu hình LoadgenHost hoặc REMOTE_HOST")
		}
		loadSSH, err := NewSSHClient(cfg.LoadgenUser, cfg.LoadgenHost, cfg.LoadgenPort, cfg.LoadgenKey, cfg.LoadgenJump)
		if err != nil {
			return nil, fmt.Errorf("lỗi cấu hình SSH máy phát tải: %w", err)
		}
		r.loadSSH = loadSSH
	}

	// 2. Khởi tạo SSH tới Server K8s (Machine 3) nếu có k8s-ssh-host
	if cfg.K8sSSHHost != "" {
		k8sSSH, err := NewSSHClient(cfg.K8sSSHUser, cfg.K8sSSHHost, cfg.K8sSSHPort, cfg.K8sSSHKey, cfg.K8sSSHJump)
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

// toolContainer trả về tên container thực thi công cụ probe / py-spy / perf theo K8sMode.
func (r *Runner) toolContainer() string {
	switch r.cfg.K8sMode {
	case "debug":
		return "capsizer-debugger"
	case "sidecar":
		if r.cfg.SidecarContainer != "" {
			return r.cfg.SidecarContainer
		}
		return "sidecar"
	default:
		return r.cfg.K8sContainer
	}
}

// PreflightCheck kiểm tra toàn diện liên kết 3 máy.
func (r *Runner) PreflightCheck(ctx context.Context) error {
	defer r.Close()

	fmt.Println("================================================================================")
	fmt.Println("         KIỂM TRA LIÊN KẾT 3 MÁY (PREFLIGHT HEALTH CHECK - GO RUNNER)")
	fmt.Println("================================================================================")

	// [1/3] Máy chạy Skill
	fmt.Println("[1/3] Máy chạy Skill (Orchestrator - Local):")
	goVer, err := exec.Command("go", "version").Output()
	if err == nil {
		fmt.Printf("      ✓ Golang: %s", string(goVer))
	}
	if r.cfg.EnvFile != "" {
		fmt.Printf("      ✓ Cấu hình hạ tầng: %s\n", r.cfg.EnvFile)
	}
	fmt.Println("      ✓ Trạng thái: Sẵn sàng điều phối")

	// [2/3] Máy phát tải
	fmt.Println("\n[2/3] Máy phát tải (Load Generator Host):")
	if r.cfg.LoadgenMode == "remote" {
		out, err := r.loadSSH.RunCommand(ctx, "uname -srm")
		if err != nil {
			return fmt.Errorf("không thể kết nối SSH tới máy phát tải: %w", err)
		}
		fmt.Printf("      ✓ Kết nối SSH thành công (%s) - OS: %s\n", r.loadSSH.DisplayTarget(), out)
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
		fmt.Printf("      ✓ Kết nối SSH server K8s thành công (%s) - OS: %s\n", r.k8sSSH.DisplayTarget(), out)
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

	// 1. Tự động tìm Pod nếu chưa có
	if r.cfg.K8sPod == "" && r.cfg.K8sSelector != "" {
		pod, err := r.k8sClient.ResolveTargetPod(ctx, r.cfg.K8sSelector, "")
		if err != nil {
			return err
		}
		r.cfg.K8sPod = pod
	}

	// In banner
	r.printBanner()

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

	// 3. Chuẩn bị môi trường công cụ trong Pod K8s (Debug Container / Sidecar / CP)
	if r.cfg.K8sPod != "" {
		if err := r.setupK8sTarget(ctx); err != nil {
			return err
		}
	}

	// 4. Đồng bộ máy phát tải từ xa nếu mode=remote
	if r.cfg.LoadgenMode == "remote" {
		fmt.Println("==> [Máy phát tải] Đồng bộ binary và kịch bản lên máy bắn tải...")
		if err := r.setupRemoteLoadgen(ctx); err != nil {
			return err
		}
	}

	// 5. [PHA 1] Đo Capacity Sizing sạch (Ramp + Probe)
	fmt.Println("\n▶ [PHA 1] KHỞI ĐỘNG ĐO CAPACITY SIZING SẠCH (KHÔNG BẬT PROFILER)...")

	execContainer := r.toolContainer()

	// Khởi chạy probe trong Pod
	if r.cfg.K8sPod != "" {
		fmt.Printf("==> Đang khởi chạy probe trong Pod %s (container: %s, port: %d)...\n",
			r.cfg.K8sPod, execContainer, r.cfg.TargetPort)
		stopOldCmd := "pkill -f 'probe -port' 2>/dev/null || pkill -x probe 2>/dev/null || true"
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, stopOldCmd)

		probeCmd := fmt.Sprintf(
			"if command -v probe >/dev/null 2>&1; then PBIN=probe; else PBIN=/tmp/probe; fi; "+
				"nohup $PBIN -port %d -target http://127.0.0.1:%d -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 < /dev/null &",
			r.cfg.TargetPort, r.cfg.TargetPort,
		)
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, probeCmd)

		defer func() {
			_, _ = r.k8sClient.ExecInPod(context.Background(), r.cfg.K8sPod, execContainer,
				"pkill -INT -f 'probe -port' 2>/dev/null || pkill -f 'probe' 2>/dev/null || true")
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
		fmt.Println("==> Đang dừng probe và tải file probe.jsonl về...")
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer,
			"pkill -INT -f 'probe -port' 2>/dev/null || pkill -f 'probe' 2>/dev/null || true")
		time.Sleep(1 * time.Second)
		if err := r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, execContainer, "/tmp/probe.jsonl", probeLog); err != nil {
			fmt.Printf("    [CẢNH BÁO] Không thể tải file probe.jsonl: %v\n", err)
		}
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
	r.LastReport = report

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

	// 6. [PHA 2] Chẩn đoán sâu tại Knee Point
	fmt.Println("\n================================================================================")
	fmt.Printf("⚠️ PHÁT HIỆN ĐIỂM GÃY TẠI MỨC TẢI: %.1f req/s\n", report.KneeRPS)
	fmt.Println("▶ [PHA 2] TỰ ĐỘNG CHUYỂN SANG CHẨN ĐOÁN NGUYÊN NHÂN SÂU (FLAMEGRAPH)...")
	fmt.Println("================================================================================")

	if r.cfg.Cooldown > 0 {
		fmt.Printf("==> Đang chờ hệ thống hạ nhiệt %ds trước khi chạy Pha 2...\n", r.cfg.Cooldown)
		time.Sleep(time.Duration(r.cfg.Cooldown) * time.Second)
	}

	profilers := r.parseProfilers(r.cfg.ProfileType)
	profDuration := 30
	totalDuration := len(profilers)*(profDuration+5) + 10

	phase2RampLog := filepath.Join(r.cfg.OutDir, "phase2-ramp.jsonl")

	// Bắn tải duy trì ở background
	bgCtx, bgCancel := context.WithCancel(ctx)
	bgDone := make(chan struct{})

	go func() {
		defer close(bgDone)
		kneeStr := fmt.Sprintf("%.1f", report.KneeRPS)
		_ = r.runLoadgen(bgCtx, kneeStr, totalDuration, 0, 0, phase2RampLog, true)
	}()

	time.Sleep(5 * time.Second)

	generatedSVGs := []string{}
	for _, prof := range profilers {
		fmt.Printf("\n--> [PROFILING: %s] Đang lấy mẫu %ds...\n", prof, profDuration)
		localSVG := filepath.Join(r.cfg.OutDir, fmt.Sprintf("flamegraph_knee_%.0frps_%s.svg", report.KneeRPS, prof))
		remoteSVG := fmt.Sprintf("/tmp/flamegraph_%s.svg", prof)

		if r.cfg.K8sPod != "" {
			if prof == "pyspy" || prof == "py-spy" {
				pyspyCmd := fmt.Sprintf(
					": > %s; PID=%d; if [ \"$PID\" -le 0 ]; then "+
						"PID=$(pgrep -f 'uvicorn|gunicorn|python|fastapi' | head -n 1); "+
						"[ -z \"$PID\" ] && PID=1; fi; "+
						"py-spy record -o %s --pid $PID --duration %d --rate 100 --subprocesses || true",
					remoteSVG, r.cfg.TargetPID, remoteSVG, profDuration,
				)
				_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, pyspyCmd)
				_ = r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, execContainer, remoteSVG, localSVG)
			} else if prof == "perf" {
				perfCmd := fmt.Sprintf(": > %s; profile-perf %d %s %d || true", remoteSVG, profDuration, remoteSVG, r.cfg.TargetPID)
				_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, perfCmd)
				_ = r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, execContainer, remoteSVG, localSVG)
			}
		}

		if fileExists(localSVG) {
			generatedSVGs = append(generatedSVGs, localSVG)
			fmt.Printf("    ✓ Đã xuất FlameGraph (%s): %s\n", prof, localSVG)
		}
		time.Sleep(3 * time.Second)
	}

	bgCancel()
	<-bgDone

	if r.cfg.LoadgenMode == "remote" && r.loadSSH != nil {
		remoteLog := fmt.Sprintf("%s/current-ramp.jsonl", r.cfg.LoadgenDir)
		_ = r.loadSSH.DownloadFile(ctx, remoteLog, phase2RampLog)
	}

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

// setupK8sTarget thiết lập container chứa công cụ trong Pod theo K8sMode (debug, sidecar, cp).
func (r *Runner) setupK8sTarget(ctx context.Context) error {
	switch r.cfg.K8sMode {
	case "debug":
		fmt.Printf("==> [K8s Target] Đang kiểm tra / gắn Ephemeral Debug Container vào Pod %s...\n", r.cfg.K8sPod)
		newPod, err := r.k8sClient.SetupDebugContainer(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, r.cfg.SidecarImage, r.cfg.K8sSelector)
		if err != nil {
			return err
		}
		r.cfg.K8sPod = newPod
	case "sidecar":
		fmt.Printf("==> [K8s Target] Sử dụng Sidecar container '%s' trong Pod %s.\n", r.toolContainer(), r.cfg.K8sPod)
	case "cp":
		fmt.Printf("==> [K8s Target] Biên dịch tĩnh và chép công cụ probe vào Pod %s...\n", r.cfg.K8sPod)
		probeBin, err := r.ensureLinuxBinary("probe", "./cmd/probe")
		if err != nil {
			return err
		}
		cName, _ := r.k8sClient.GetContainerName(ctx, r.cfg.K8sPod, r.cfg.K8sContainer)
		r.cfg.K8sContainer = cName
		if err := r.k8sClient.CopyToPod(ctx, probeBin, r.cfg.K8sPod, cName, "/tmp/probe"); err != nil {
			return fmt.Errorf("không thể chép probe vào Pod: %w", err)
		}
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, cName, "chmod +x /tmp/probe")
	}
	return nil
}

func (r *Runner) ensureLinuxBinary(name, pkgPath string) (string, error) {
	root := r.cfg.ProjectRoot
	if root == "" {
		root = "."
	}
	binDir := filepath.Join(root, "bin")
	_ = os.MkdirAll(binDir, 0755)
	outBin := filepath.Join(binDir, name+"-linux-amd64")

	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", outBin, pkgPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("biên dịch %s thất bại: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return outBin, nil
}

func (r *Runner) ensureLocalRampBinary() (string, error) {
	root := r.cfg.ProjectRoot
	if root == "" {
		root = "."
	}
	localBin := filepath.Join(root, "bin", "capsizer-ramp")
	if fileExists(localBin) {
		return localBin, nil
	}
	if pathBin, err := exec.LookPath("capsizer-ramp"); err == nil {
		return pathBin, nil
	}
	_ = os.MkdirAll(filepath.Dir(localBin), 0755)
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", localBin, "./cmd/ramp")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("không thể biên dịch capsizer-ramp: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return localBin, nil
}

func (r *Runner) runLoadgen(ctx context.Context, steps string, stepSec, baseWait, cooldown int, outPath string, background bool) error {
	if r.cfg.LoadgenMode == "remote" {
		remoteRamp := fmt.Sprintf("%s/bin/capsizer-ramp", r.cfg.LoadgenDir)
		remoteLog := fmt.Sprintf("%s/current-ramp.jsonl", r.cfg.LoadgenDir)

		cmdParts := []string{
			quoteShellArg(remoteRamp),
			"-steps", quoteShellArg(steps),
			"-step-seconds", fmt.Sprintf("%d", stepSec),
			"-baseline-wait", fmt.Sprintf("%d", baseWait),
			"-cooldown", fmt.Sprintf("%d", cooldown),
			"-timeout", fmt.Sprintf("%d", r.cfg.Timeout),
			"-max-inflight", fmt.Sprintf("%d", r.cfg.MaxInflight),
			"-out", quoteShellArg(remoteLog),
		}

		if r.cfg.Scenario != "" {
			remoteScen := fmt.Sprintf("%s/scenarios/%s", r.cfg.LoadgenDir, r.cfg.Scenario)
			cmdParts = append(cmdParts, "-scenario", quoteShellArg(remoteScen))
		}
		if r.cfg.URL != "" {
			cmdParts = append(cmdParts, "-url", quoteShellArg(r.cfg.URL))
		}
		if r.cfg.Method != "" {
			cmdParts = append(cmdParts, "-method", quoteShellArg(r.cfg.Method))
		}
		if r.cfg.Body != "" {
			cmdParts = append(cmdParts, "-body", quoteShellArg(r.cfg.Body))
		}
		if r.cfg.BodyFile != "" {
			remoteBodyFile := fmt.Sprintf("%s/payload.body", r.cfg.LoadgenDir)
			if r.cfg.ScenarioDir != "" && strings.HasPrefix(r.cfg.BodyFile, r.cfg.ScenarioDir) {
				remoteBodyFile = fmt.Sprintf("%s/scenarios/%s/%s", r.cfg.LoadgenDir, r.cfg.Scenario, filepath.Base(r.cfg.BodyFile))
			}
			cmdParts = append(cmdParts, "-body-file", quoteShellArg(remoteBodyFile))
		}
		if r.cfg.CacheBust {
			cmdParts = append(cmdParts, "-cache-bust")
		}
		if r.cfg.SkipVerify {
			cmdParts = append(cmdParts, "-skip-verify")
		}
		for _, opt := range r.cfg.ExtraOpts {
			cmdParts = append(cmdParts, "-opt", quoteShellArg(opt))
		}

		fullCmd := strings.Join(cmdParts, " ")

		if background {
			_, err := r.loadSSH.RunCommand(ctx, fullCmd)
			return err
		}

		out, err := r.loadSSH.RunCommand(ctx, fullCmd)
		if out != "" {
			fmt.Println(out)
		}
		if err == nil {
			if dlErr := r.loadSSH.DownloadFile(ctx, remoteLog, outPath); dlErr != nil {
				return fmt.Errorf("không thể tải file kết quả ramp từ máy phát tải: %w", dlErr)
			}
		}
		return err
	}

	// Chạy cục bộ bằng capsizer-ramp
	rampBin, err := r.ensureLocalRampBinary()
	if err != nil {
		return err
	}

	args := []string{
		"-steps", steps,
		"-step-seconds", fmt.Sprintf("%d", stepSec),
		"-baseline-wait", fmt.Sprintf("%d", baseWait),
		"-cooldown", fmt.Sprintf("%d", cooldown),
		"-timeout", fmt.Sprintf("%d", r.cfg.Timeout),
		"-max-inflight", fmt.Sprintf("%d", r.cfg.MaxInflight),
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
	if r.cfg.Method != "" {
		args = append(args, "-method", r.cfg.Method)
	}
	if r.cfg.Body != "" {
		args = append(args, "-body", r.cfg.Body)
	}
	if r.cfg.BodyFile != "" {
		args = append(args, "-body-file", r.cfg.BodyFile)
	}
	if r.cfg.CacheBust {
		args = append(args, "-cache-bust")
	}
	if r.cfg.SkipVerify {
		args = append(args, "-skip-verify")
	}
	for _, opt := range r.cfg.ExtraOpts {
		args = append(args, "-opt", opt)
	}

	cmd := exec.CommandContext(ctx, rampBin, args...)
	if !background {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

func (r *Runner) setupRemoteLoadgen(ctx context.Context) error {
	// Mở rộng đường dẫn ~ trên máy phát tải từ xa thành đường dẫn tuyệt đối
	if strings.HasPrefix(r.cfg.LoadgenDir, "~/") {
		if remoteHome, err := r.loadSSH.RunCommand(ctx, "pwd"); err == nil && remoteHome != "" {
			r.cfg.LoadgenDir = strings.TrimRight(remoteHome, "/") + "/" + strings.TrimPrefix(r.cfg.LoadgenDir, "~/")
		}
	} else if r.cfg.LoadgenDir == "~" {
		if remoteHome, err := r.loadSSH.RunCommand(ctx, "pwd"); err == nil && remoteHome != "" {
			r.cfg.LoadgenDir = strings.TrimRight(remoteHome, "/")
		}
	}

	remoteDir := r.cfg.LoadgenDir
	mkdirCmd := fmt.Sprintf("mkdir -p %s/bin", quoteShellArg(remoteDir))
	if r.cfg.Scenario != "" {
		mkdirCmd = fmt.Sprintf("mkdir -p %s/bin %s/scenarios/%s",
			quoteShellArg(remoteDir), quoteShellArg(remoteDir), quoteShellArg(r.cfg.Scenario))
	}
	if _, err := r.loadSSH.RunCommand(ctx, mkdirCmd); err != nil {
		return fmt.Errorf("không thể tạo thư mục trên máy phát tải: %w", err)
	}

	// 1. Biên dịch tĩnh capsizer-ramp và tải lên Machine 2
	rampBin, err := r.ensureLinuxBinary("capsizer-ramp", "./cmd/ramp")
	if err != nil {
		return err
	}
	remoteRamp := fmt.Sprintf("%s/bin/capsizer-ramp", remoteDir)
	if err := r.loadSSH.UploadFile(ctx, rampBin, remoteRamp); err != nil {
		return fmt.Errorf("không thể tải binary capsizer-ramp lên máy phát tải: %w", err)
	}
	_, _ = r.loadSSH.RunCommand(ctx, fmt.Sprintf("chmod +x %s", quoteShellArg(remoteRamp)))

	// 2. Đồng bộ kịch bản (scenario.json, datasets, ...)
	if r.cfg.ScenarioDir != "" {
		entries, err := os.ReadDir(r.cfg.ScenarioDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					localF := filepath.Join(r.cfg.ScenarioDir, e.Name())
					remoteF := fmt.Sprintf("%s/scenarios/%s/%s", remoteDir, r.cfg.Scenario, e.Name())
					if err := r.loadSSH.UploadFile(ctx, localF, remoteF); err != nil {
						return fmt.Errorf("lỗi tải file kịch bản %s lên máy phát tải: %w", e.Name(), err)
					}
				}
			}
		}
	}

	// 3. Đồng bộ BodyFile nếu nằm ngoài thư mục kịch bản
	if r.cfg.BodyFile != "" && fileExists(r.cfg.BodyFile) {
		if r.cfg.ScenarioDir == "" || !strings.HasPrefix(r.cfg.BodyFile, r.cfg.ScenarioDir) {
			remoteBody := fmt.Sprintf("%s/payload.body", remoteDir)
			_ = r.loadSSH.UploadFile(ctx, r.cfg.BodyFile, remoteBody)
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
	if r.cfg.LoadgenMode == "remote" && r.loadSSH != nil {
		fmt.Printf("[2] Máy phát tải      : REMOTE (%s)\n", r.loadSSH.DisplayTarget())
	} else {
		fmt.Println("[2] Máy phát tải      : LOCAL (Chính máy này)")
	}
	if r.k8sSSH != nil {
		fmt.Printf("[3] Server cụm K8s    : SSH %s -> Pod %s/%s (mode: %s)\n",
			r.k8sSSH.DisplayTarget(), r.cfg.K8sNamespace, r.cfg.K8sPod, r.cfg.K8sMode)
	} else if r.cfg.K8sPod != "" {
		fmt.Printf("[3] Cụm K8s mục tiêu  : Local Kubectl -> Pod %s/%s (mode: %s)\n",
			r.cfg.K8sNamespace, r.cfg.K8sPod, r.cfg.K8sMode)
	}
	fmt.Printf("Endpoint nhận request : %s (Scenario: %s, TargetPort: %d)\n", r.cfg.URL, r.cfg.Scenario, r.cfg.TargetPort)
	fmt.Printf("Dải bậc tải           : %s (Mỗi bậc: %ds, Cooldown: %ds)\n", r.cfg.Steps, r.cfg.StepSeconds, r.cfg.Cooldown)
	if r.cfg.SetPodCPU != "" {
		fmt.Printf("Cấu hình CPU Pod      : Tự động đưa về %s core (Khôi phục khi xong: %v)\n", r.cfg.SetPodCPU, r.cfg.RestorePodCPU)
	}
	fmt.Printf("Thư mục output        : %s\n", r.cfg.OutDir)
	fmt.Println("================================================================================")
}
