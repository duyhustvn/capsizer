package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"capsizer/pkg/analyzer"
)

// Runner thực thi quy trình chuẩn 2 Pha định chuẩn năng lực & profiling.
type Runner struct {
	cfg                *Config
	loadSSH            *SSHClient
	k8sSSH             *SSHClient
	k8sClient          *K8sClient
	debugContainerName string
	LastReport         *analyzer.AnalysisReport
}

// NewRunner khởi tạo Runner từ Config.
func NewRunner(cfg *Config) (*Runner, error) {
	cfg.Finalize()
	r := &Runner{
		cfg:                cfg,
		debugContainerName: "capsizer-debugger",
	}

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
		if r.debugContainerName != "" {
			return r.debugContainerName
		}
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

	var (
		targetWorkloadType string
		targetWorkloadName string
		targetContainerIdx int
		origResourcesJSON  string
		probesBackup       *WorkloadProbesBackup
	)

	// Defer hoàn nguyên đồng thời cấu hình CPU và Liveness/Readiness Probes trong 1 lần rollout duy nhất khi kết thúc
	defer func() {
		restoreCPU := ""
		if r.cfg.RestorePodCPU {
			restoreCPU = origResourcesJSON
		}
		if restoreCPU == "" && probesBackup == nil {
			return
		}
		fmt.Println("\n==> [Khôi phục Hạ tầng] Đang hoàn nguyên cấu hình Workload (CPU / Liveness & Readiness Probes) ban đầu...")
		if err := r.k8sClient.RestoreWorkloadState(context.Background(), targetWorkloadType, targetWorkloadName, targetContainerIdx, restoreCPU, probesBackup); err != nil {
			fmt.Printf("    [CẢNH BÁO] Lỗi khi hoàn nguyên cấu hình Workload: %v\n", err)
		} else {
			fmt.Println("    ✓ Đã hoàn nguyên cấu hình Workload thành công.")
		}
	}()

	// 2. Điều chỉnh CPU Pod nếu có yêu cầu (Scale to 1 Core)
	if r.cfg.SetPodCPU != "" && r.cfg.K8sPod != "" {
		cName, _ := r.k8sClient.GetContainerName(ctx, r.cfg.K8sPod, r.cfg.K8sContainer)
		r.cfg.K8sContainer = cName

		wType, wName, err := r.k8sClient.DetectTargetWorkload(ctx, r.cfg.K8sPod, r.cfg.K8sSelector)
		if err == nil {
			targetWorkloadType = wType
			targetWorkloadName = wName
			fmt.Printf("==> [Cấu hình CPU] Đang đưa %s/%s về %s core...\n", wType, wName, r.cfg.SetPodCPU)
			origJSON, cIdx, err := r.k8sClient.ApplyWorkloadCPU(ctx, wType, wName, cName, r.cfg.SetPodCPU)
			if err != nil {
				fmt.Printf("    [CẢNH BÁO] Không thể điều chỉnh CPU: %v\n", err)
			} else {
				origResourcesJSON = origJSON
				targetContainerIdx = cIdx
				fmt.Printf("    ✓ Đã cấu hình CPU %s core (Bảo lưu: %s)\n", r.cfg.SetPodCPU, origJSON)

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
		r.waitAppStable(ctx, r.toolContainer(), 3, 90*time.Second)
	}

	// 4. Đồng bộ máy phát tải từ xa nếu mode=remote
	if r.cfg.LoadgenMode == "remote" {
		fmt.Println("==> [Máy phát tải] Đồng bộ binary và kịch bản lên máy bắn tải...")
		if err := r.setupRemoteLoadgen(ctx); err != nil {
			return err
		}
	}

	// 4.5. Làm nóng ứng dụng bằng request kịch bản thật (tránh cold-start như giải nén langid/BM25 ở bậc đầu Pha 1)
	r.warmupApplication(ctx, r.toolContainer())

	// 5. [PHA 1] Đo Capacity Sizing sạch (Ramp + Probe)
	fmt.Println("\n▶ [PHA 1] KHỞI ĐỘNG ĐO CAPACITY SIZING SẠCH (KHÔNG BẬT PROFILER)...")

	rampLog := filepath.Join(r.cfg.OutDir, "phase1-ramp.jsonl")
	probeLog := filepath.Join(r.cfg.OutDir, "phase1-probe.jsonl")
	reportLog := filepath.Join(r.cfg.OutDir, "phase1-report.txt")

	execContainer := r.toolContainer()

	// Ghi nhận restartCount của container ứng dụng trước khi chạy Pha 1
	var initialRestartCount int
	if r.cfg.K8sPod != "" {
		if preSt, err := r.k8sClient.GetPodContainerStatus(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, execContainer); err == nil {
			initialRestartCount = preSt.RestartCount
		}
	}

	var (
		streamedSamples    atomic.Int64
		podCrashedInPhase1 atomic.Bool
		crashDetailMu      sync.Mutex
		crashDetail        string
		probeStreamCancel  context.CancelFunc
		probeStreamDone    = make(chan struct{})
	)

	// Khởi chạy probe trong Pod và mở luồng stream thời gian thực về phase1-probe.jsonl
	if r.cfg.K8sPod != "" {
		fmt.Printf("==> Đang khởi chạy probe trong Pod %s (container: %s, port: %d) và stream thời gian thực về máy Skill...\n",
			r.cfg.K8sPod, execContainer, r.cfg.TargetPort)

		stopOldCmd := "pkill -INT -x probe 2>/dev/null || pkill -x probe 2>/dev/null || true"
		_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, stopOldCmd)

		probeStartCmd := fmt.Sprintf(
			"if [ -x /tmp/probe ]; then PBIN=/tmp/probe; elif command -v probe >/dev/null 2>&1; then PBIN=probe; else PBIN=/tmp/probe; fi; "+
				": > /tmp/probe.jsonl; "+
				"nohup $PBIN -port %d -target http://127.0.0.1:%d -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 < /dev/null & "+
				"sleep 2; pgrep -x probe >/dev/null && echo PROBE_RUNNING || (cat /tmp/probe.log 2>/dev/null; exit 1)",
			r.cfg.TargetPort, r.cfg.TargetPort,
		)
		if out, err := r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, probeStartCmd); err != nil {
			fmt.Printf("    [CẢNH BÁO] Lỗi khởi chạy probe trong Pod (%s): %v (output: %s)\n", execContainer, err, out)
		} else {
			fmt.Println("    ✓ Tiến trình probe đã chạy trong Pod.")
		}

		defer func() {
			_, _ = r.k8sClient.ExecInPod(context.Background(), r.cfg.K8sPod, r.toolContainer(),
				"pkill -INT -x probe 2>/dev/null || pkill -x probe 2>/dev/null || true")
		}()

		// Mở luồng stream trực tiếp từng dòng từ /tmp/probe.jsonl về local probeLog (kèm cơ chế tự động retry + chống trùng dòng)
		var streamCtx context.Context
		streamCtx, probeStreamCancel = context.WithCancel(ctx)
		defer probeStreamCancel()
		go func() {
			defer close(probeStreamDone)

			probeFile, err := os.OpenFile(probeLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				fmt.Printf("    [CẢNH BÁO] Không thể mở file %s để ghi stream probe: %v\n", probeLog, err)
				return
			}
			defer probeFile.Close()

			var probeMu sync.Mutex
			seenLines := make(map[string]bool)

			writeProbeLine := func(line string) {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
					return
				}
				probeMu.Lock()
				if seenLines[trimmed] {
					probeMu.Unlock()
					return
				}
				seenLines[trimmed] = true
				_, _ = probeFile.WriteString(trimmed + "\n")
				_ = probeFile.Sync()
				probeMu.Unlock()

				count := streamedSamples.Add(1)
				if count == 1 {
					fmt.Println("    ✓ Luồng stream probe đã kết nối và nhận mẫu đầu tiên thành công.")
				}
			}

			syncSnapshot := func(sCtx context.Context) {
				cCtx, cancel := context.WithTimeout(sCtx, 8*time.Second)
				defer cancel()
				if snapOut, snapErr := r.k8sClient.ExecInPod(cCtx, r.cfg.K8sPod, execContainer, "cat /tmp/probe.jsonl 2>/dev/null || true"); snapErr == nil && snapOut != "" {
					for _, l := range strings.Split(snapOut, "\n") {
						writeProbeLine(l)
					}
				}
			}

			handleCrashDetected := func(curSt *PodContainerStatus) {
				if podCrashedInPhase1.Swap(true) {
					return
				}
				msg := fmt.Sprintf(
					"Container ứng dụng bị K8s kill/restart giữa Pha 1 (restartCount: %d -> %d, reason: %s, exitCode: %d)",
					initialRestartCount, curSt.RestartCount, curSt.LastTermReason, curSt.LastTermExitCode,
				)
				crashDetailMu.Lock()
				crashDetail = msg
				crashDetailMu.Unlock()

				fmt.Printf("\n================================================================================\n")
				fmt.Printf("⚠️ [PHÁT HIỆN SẬP CONTAINER TRONG PHA 1]\n")
				fmt.Printf("  • %s\n", msg)
				fmt.Printf("  • Ephemeral container (%s) chia sẻ PID namespace nên đã bị SIGKILL theo.\n", execContainer)
				fmt.Printf("  • Đã bảo toàn %d mẫu probe.jsonl theo thời gian thực tính đến giây cuối trước khi sập!\n", streamedSamples.Load())
				fmt.Printf("  • Đang gửi tín hiệu dừng sớm Pha 1 để tổng hợp báo cáo...\n")
				fmt.Printf("================================================================================\n")

				probeStreamCancel()
				r.stopActiveLoadgen()
			}

			// Goroutine phụ trợ chạy định kỳ mỗi 10s:
			// 1) Đồng bộ dự phòng /tmp/probe.jsonl (phòng trường hợp tail -f bị treo/buffer qua ProxyCommand mà không báo EOF)
			// 2) Kiểm tra trạng thái container (phòng trường hợp container chết nhưng phiên SSH stream bị treo không thoát)
			go func() {
				ticker := time.NewTicker(10 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
						chkCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
						curSt, stErr := r.k8sClient.GetPodContainerStatus(chkCtx, r.cfg.K8sPod, r.cfg.K8sContainer, execContainer)
						cancel()
						if stErr == nil && (curSt.RestartCount > initialRestartCount || !curSt.DebugRunning) {
							handleCrashDetected(curSt)
							return
						}
						syncSnapshot(streamCtx)
					}
				}
			}()

			for attempt := 1; streamCtx.Err() == nil; attempt++ {
				streamErr := r.k8sClient.StreamExecInPod(streamCtx, r.cfg.K8sPod, execContainer, "tail -n +1 -f /tmp/probe.jsonl", writeProbeLine)
				if streamCtx.Err() != nil {
					break
				}

				// Luôn in rõ nguyên nhân khi stream bị ngắt (không bao giờ chết im lặng)
				fmt.Printf("\n    [CẢNH BÁO Stream Probe] Luồng stream bị ngắt (lần %d, đã lưu %d mẫu): %v\n",
					attempt, streamedSamples.Load(), streamErr)

				// Kiểm tra ngay xem có phải container ứng dụng vừa bị LivenessProbe / OOM kill không
				curSt, stErr := r.k8sClient.GetPodContainerStatus(context.Background(), r.cfg.K8sPod, r.cfg.K8sContainer, execContainer)
				if stErr == nil && (curSt.RestartCount > initialRestartCount || !curSt.DebugRunning) {
					handleCrashDetected(curSt)
					return
				}

				// Nếu Pod và debug container vẫn sống khỏe mà stream bị ngắt (do nghẽn mạng / SSH / API):
				// 1. Đồng bộ ngay toàn bộ các dòng hiện có trong /tmp/probe.jsonl
				syncSnapshot(streamCtx)

				// 2. Đảm bảo tiến trình probe trong container vẫn đang chạy
				ensureCmd := fmt.Sprintf(
					"if ! pgrep -x probe >/dev/null 2>&1; then "+
						"if [ -x /tmp/probe ]; then PBIN=/tmp/probe; else PBIN=probe; fi; "+
						"nohup $PBIN -port %d -target http://127.0.0.1:%d -out /tmp/probe.jsonl >>/tmp/probe.log 2>&1 < /dev/null & "+
						"fi",
					r.cfg.TargetPort, r.cfg.TargetPort,
				)
				_, _ = r.k8sClient.ExecInPod(streamCtx, r.cfg.K8sPod, execContainer, ensureCmd)

				// 3. Chờ backoff 2s trước khi kết nối lại stream
				select {
				case <-streamCtx.Done():
					return
				case <-time.After(2 * time.Second):
					fmt.Printf("    • Đang kết nối lại luồng stream probe (lần %d)...\n", attempt+1)
				}
			}

			// Khi Pha 1 kết thúc bình thường (không crash), vét lần cuối toàn bộ /tmp/probe.jsonl
			if !podCrashedInPhase1.Load() {
				syncSnapshot(context.Background())
			}
		}()
	} else {
		close(probeStreamDone)
	}

	time.Sleep(1 * time.Second)

	// Chạy Ramp Pha 1
	fmt.Printf("==> Bắt đầu phát tải Open-Loop (đếm lùi %ds lấy mẫu nền)...\n", r.cfg.BaselineWait)
	if err := r.runLoadgen(ctx, r.cfg.Steps, r.cfg.StepSeconds, r.cfg.BaselineWait, r.cfg.Cooldown, rampLog, false); err != nil {
		if !podCrashedInPhase1.Load() {
			return fmt.Errorf("lỗi phát tải Pha 1: %w", err)
		}
		fmt.Printf("    [!] Phát tải Pha 1 đã dừng sớm do sự cố sập container (%v).\n", err)
	} else {
		fmt.Println("==> Hoàn thành phát tải Pha 1!")
	}

	// Dừng probe và đóng stream
	if r.cfg.K8sPod != "" {
		if !podCrashedInPhase1.Load() {
			_, _ = r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer,
				"pkill -INT -x probe 2>/dev/null || pkill -x probe 2>/dev/null || true")
			time.Sleep(1 * time.Second)
		}
		if probeStreamCancel != nil {
			probeStreamCancel()
		}
		<-probeStreamDone

		// Kiểm tra lại trạng thái Pod sau Pha 1 nếu chưa ghi nhận crash
		if !podCrashedInPhase1.Load() {
			if postSt, err := r.k8sClient.GetPodContainerStatus(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, execContainer); err == nil {
				if postSt.RestartCount > initialRestartCount || !postSt.DebugRunning {
					podCrashedInPhase1.Store(true)
					crashDetailMu.Lock()
					crashDetail = fmt.Sprintf(
						"Container ứng dụng bị K8s kill/restart trong Pha 1 (restartCount: %d -> %d, reason: %s, exitCode: %d)",
						initialRestartCount, postSt.RestartCount, postSt.LastTermReason, postSt.LastTermExitCode,
					)
					crashDetailMu.Unlock()
				}
			}
		}

		// Nếu vì lý do nào đó stream chưa nhận được mẫu mà debug container vẫn còn sống, thử tải trực tiếp
		if streamedSamples.Load() == 0 && !podCrashedInPhase1.Load() {
			fmt.Println("==> Đang tải file probe.jsonl từ Pod về...")
			if err := r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, execContainer, "/tmp/probe.jsonl", probeLog); err != nil {
				fmt.Printf("    [CẢNH BÁO] Không thể tải file probe.jsonl: %v\n", err)
			}
		} else {
			fmt.Printf("==> Đã thu thập và lưu an toàn %d mẫu probe tại: %s\n", streamedSamples.Load(), probeLog)
		}
	}

	// Phân tích kết quả Pha 1 với pkg/analyzer
	fmt.Println("\n==> Đang phân tích kết quả định chuẩn Pha 1...")
	rampRecords, err := analyzer.LoadRamp(rampLog)
	if err != nil {
		return fmt.Errorf("không đọc được file ramp (%s): %w", rampLog, err)
	}

	var probeRecords []analyzer.ProbeRecord
	if fileExists(probeLog) {
		probeRecords, err = analyzer.LoadProbe(probeLog)
		if err != nil {
			fmt.Printf("    [CẢNH BÁO] Lỗi đọc file probe (%s): %v\n", probeLog, err)
		}
	}
	if r.cfg.K8sPod != "" && len(probeRecords) == 0 {
		fmt.Println("    [CẢNH BÁO QUAN TRỌNG] Không có mẫu dữ liệu nào trong probe.jsonl! Các cột CPU/RAM/TCP/Health trong bảng sẽ hiển thị '-'.")
	}

	report, err := analyzer.Analyze(rampRecords, probeRecords)
	if err != nil {
		return fmt.Errorf("phân tích sizing thất bại: %w", err)
	}

	// Nếu container bị kill giữa Pha 1, bổ sung thông tin sự cố vào báo cáo và đảm bảo nhận diện Knee Point
	crashDetailMu.Lock()
	cDetail := crashDetail
	crashDetailMu.Unlock()
	if podCrashedInPhase1.Load() && cDetail != "" {
		crashBanner := fmt.Sprintf(
			"⚠️ CẢNH BÁO SỰ CỐ HẠ TẦNG TRONG PHA 1:\n   %s\n   (Dữ liệu probe.jsonl đã được bảo toàn đến thời điểm ngay trước khi container bị kill)\n\n",
			cDetail,
		)
		report.ReportText = crashBanner + report.ReportText
		if !report.HasKnee && report.LastOkRPS > 0 {
			report.HasKnee = true
			report.KneeRPS = report.LastOkRPS
			report.ProfileRPS = report.LastOkRPS
			report.KneeReason = cDetail
		}
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

	profRPS := report.ProfileRPS
	if profRPS <= 0 {
		profRPS = report.KneeRPS
	}

	// 6. [PHA 2] Chẩn đoán sâu tại Knee Point
	fmt.Println("\n================================================================================")
	fmt.Printf("⚠️ PHÁT HIỆN ĐIỂM GÃY TẠI MỨC TẢI: %.1f req/s\n", report.KneeRPS)
	if profRPS != report.KneeRPS {
		fmt.Printf("🎯 MỨC TẢI PROFILING PHA 2      : %.1f req/s (vùng ứng dụng còn sống và chịu áp lực thực tế)\n", profRPS)
	}
	fmt.Println("▶ [PHA 2] TỰ ĐỘNG CHUYỂN SANG CHẨN ĐOÁN NGUYÊN NHÂN SÂU (FLAMEGRAPH)...")
	fmt.Println("================================================================================")

	probesRelaxed := false
	if r.cfg.K8sPod != "" {
		if r.cfg.K8sContainer == "" {
			if cName, err := r.k8sClient.GetContainerName(ctx, r.cfg.K8sPod, ""); err == nil {
				r.cfg.K8sContainer = cName
			}
		}
		if targetWorkloadType == "" || targetWorkloadName == "" {
			if wType, wName, err := r.k8sClient.DetectTargetWorkload(ctx, r.cfg.K8sPod, r.cfg.K8sSelector); err == nil {
				targetWorkloadType = wType
				targetWorkloadName = wName
			}
		}

		// [PHƯƠNG ÁN 1] Tạm thời nới lỏng livenessProbe & readinessProbe trước Pha 2
		// (failureThreshold=60, timeoutSeconds>=10s) để Kubelet không kill Pod và không rút Endpoint
		// khỏi Service trong suốt thời gian giữ tải tại Knee Point để chạy py-spy & perf!
		if targetWorkloadType != "" && targetWorkloadName != "" {
			fmt.Printf("==> [Cấu hình Probes Pha 2] Đang tạm nới lỏng livenessProbe & readinessProbe của %s/%s (failureThreshold=60, timeoutSeconds>=10s)...\n",
				targetWorkloadType, targetWorkloadName)
			pb, err := r.k8sClient.RelaxWorkloadProbes(ctx, targetWorkloadType, targetWorkloadName, r.cfg.K8sContainer)
			if pb != nil {
				probesBackup = pb
			}
			if err != nil {
				fmt.Printf("    [CẢNH BÁO] Không thể nới lỏng Probes cho Pha 2 (%v), chuyển sang Guard dự phòng.\n", err)
			} else if pb != nil {
				probesRelaxed = true
				fmt.Println("    ✓ Đã nới lỏng livenessProbe & readinessProbe (sẽ tự động hoàn nguyên khi kết thúc).")
				fmt.Println("    • Đang chờ Pod mới (với Probes đã nới lỏng) Ready sau rollout...")
				if newPod, wErr := r.k8sClient.WaitForPodReady(ctx, r.cfg.K8sSelector, 180*time.Second); wErr == nil && newPod != "" {
					r.cfg.K8sPod = newPod
					fmt.Printf("    ✓ Pod mới đã sẵn sàng cho Pha 2: %s\n", newPod)
				}
				if sErr := r.setupK8sTarget(ctx); sErr != nil {
					return fmt.Errorf("[Pha 2] Không thể thiết lập container công cụ trên Pod mới %s: %w", r.cfg.K8sPod, sErr)
				}
				execContainer = r.toolContainer()
				r.waitAppStable(ctx, execContainer, 2, 60*time.Second)
				// Làm nóng lại Pod mới vừa rollout bằng request kịch bản thật để tránh chụp nhầm cold-start (bz2.decompress / langid)
				r.warmupApplication(ctx, execContainer)
			} else {
				fmt.Println("    • Workload không khai báo livenessProbe/readinessProbe, bỏ qua bước patch Probes.")
			}
		}
	}

	if !probesRelaxed {
		if r.cfg.Cooldown > 0 {
			fmt.Printf("==> Đang chờ hệ thống hạ nhiệt %ds trước khi chạy Pha 2...\n", r.cfg.Cooldown)
			time.Sleep(time.Duration(r.cfg.Cooldown) * time.Second)
		}

		// [GUARD PHA 2] Kiểm tra sức khỏe Pod & tự động hồi phục Debug Container nếu bị kill ở Pha 1
		if r.cfg.K8sPod != "" {
			fmt.Println("==> [Guard Pha 2] Đang kiểm tra trạng thái Pod và container công cụ trước khi profiling...")
			if r.cfg.K8sMode == "debug" {
				newPod, activeDbg, err := r.k8sClient.EnsureDebugContainerAlive(
					ctx, r.cfg.K8sPod, r.cfg.K8sContainer, r.cfg.SidecarImage, r.cfg.K8sSelector, r.debugContainerName,
				)
				if err != nil {
					return fmt.Errorf("[Guard Pha 2] Không thể thực hiện profiling do Pod không khả dụng: %w", err)
				}
				r.cfg.K8sPod = newPod
				r.debugContainerName = activeDbg
				execContainer = r.toolContainer()
				fmt.Printf("    ✓ Guard Pha 2 OK: Pod %s sẵn sàng, container công cụ '%s' đang running.\n", r.cfg.K8sPod, execContainer)
			} else {
				st, err := r.k8sClient.GetPodContainerStatus(ctx, r.cfg.K8sPod, r.cfg.K8sContainer, execContainer)
				if err != nil || !st.PodReady {
					return fmt.Errorf("[Guard Pha 2] Pod %s chưa Ready (Waiting=%s, RestartCount=%d) — dừng Pha 2",
						r.cfg.K8sPod, st.WaitingReason, st.RestartCount)
				}
				if r.cfg.K8sMode == "cp" && podCrashedInPhase1.Load() {
					// Nếu container ứng dụng vừa restart ở chế độ cp, cần chép lại binary /tmp/probe nếu cần
					_ = r.setupK8sTarget(ctx)
				}
			}
			r.waitAppStable(ctx, execContainer, 2, 60*time.Second)
			if podCrashedInPhase1.Load() {
				r.warmupApplication(ctx, execContainer)
			}
		}
	}

	profilers := r.parseProfilers(r.cfg.ProfileType)
	profDuration := 30
	warmupSec := 5

	// Chỉ cần rút ngắn thời gian lấy mẫu xuống 12s nếu container từng bị kill ở Pha 1 VÀ không thể nới lỏng LivenessProbe
	if podCrashedInPhase1.Load() && !probesRelaxed {
		profDuration = 12
		warmupSec = 2
		fmt.Printf("    [!] Do container từng bị kill ở Pha 1 và không patch được LivenessProbe, tự động tối ưu thời gian lấy mẫu xuống %ds (warmup %ds)!\n",
			profDuration, warmupSec)
	}

	totalDuration := len(profilers)*(profDuration+5) + warmupSec + 5
	phase2RampLog := filepath.Join(r.cfg.OutDir, "phase2-ramp.jsonl")

	// Bắn tải duy trì ở background
	bgCtx, bgCancel := context.WithCancel(ctx)
	bgDone := make(chan struct{})

	go func() {
		defer close(bgDone)
		kneeStr := fmt.Sprintf("%.1f", profRPS)
		_ = r.runLoadgen(bgCtx, kneeStr, totalDuration, 0, 0, phase2RampLog, true)
	}()

	time.Sleep(time.Duration(warmupSec) * time.Second)

	generatedSVGs := []string{}
	for _, prof := range profilers {
		fmt.Printf("\n--> [PROFILING: %s] Đang lấy mẫu %ds trong container '%s' (tại %.1f req/s)...\n", prof, profDuration, execContainer, profRPS)
		localSVG := filepath.Join(r.cfg.OutDir, fmt.Sprintf("flamegraph_knee_%.0frps_%s.svg", profRPS, prof))
		remoteSVG := fmt.Sprintf("/tmp/flamegraph_%s.svg", prof)

		if r.cfg.K8sPod != "" {
			pidResolveSnippet := fmt.Sprintf(
				"PID=%d; if [ \"$PID\" -le 0 ]; then "+
					"PID=$(pgrep -f 'uvicorn|gunicorn|python|fastapi' | grep -vw \"$$\" | head -n 1); "+
					"fi; "+
					"case \"$PID\" in ''|*[!0-9]*) PID=1 ;; esac; "+
					"echo \"Target PID=$PID\"",
				r.cfg.TargetPID,
			)
			var profCmd string
			if prof == "pyspy" || prof == "py-spy" {
				profCmd = fmt.Sprintf(
					": > %s; %s; py-spy record -o %s --pid \"$PID\" --duration %d --rate 100 --subprocesses",
					remoteSVG, pidResolveSnippet, remoteSVG, profDuration,
				)
			} else if prof == "perf" {
				profCmd = fmt.Sprintf(
					": > %s; %s; profile-perf -p \"$PID\" -d %d -o %s",
					remoteSVG, pidResolveSnippet, profDuration, remoteSVG,
				)
			}

			if profCmd != "" {
				out, execErr := r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, profCmd)
				if execErr != nil {
					fmt.Printf("    [CẢNH BÁO] Lệnh profiler (%s) báo lỗi: %v\n", prof, execErr)
					if out != "" {
						fmt.Printf("    [Chi tiết profiler output]: %s\n", out)
					}
				}

				if cpErr := r.k8sClient.CopyFromPod(ctx, r.cfg.K8sPod, execContainer, remoteSVG, localSVG); cpErr != nil {
					fmt.Printf("    [ERROR] Không thể tải FlameGraph (%s) từ %s:%s về máy: %v\n",
						prof, execContainer, remoteSVG, cpErr)
					if out != "" && execErr == nil {
						fmt.Printf("    [Chi tiết profiler output]: %s\n", out)
					}
				}
			}
		}

		if isValidSVGFile(localSVG) {
			generatedSVGs = append(generatedSVGs, localSVG)
			fmt.Printf("    ✓ Đã xuất FlameGraph (%s): %s\n", prof, localSVG)
		} else {
			fmt.Printf("    ✗ Không tạo được file FlameGraph hợp lệ cho profiler '%s'.\n", prof)
		}
		time.Sleep(2 * time.Second)
	}

	bgCancel()
	r.stopActiveLoadgen()
	<-bgDone

	if r.cfg.LoadgenMode == "remote" && r.loadSSH != nil {
		remoteLog := fmt.Sprintf("%s/current-ramp.jsonl", r.cfg.LoadgenDir)
		_ = r.loadSSH.DownloadFile(ctx, remoteLog, phase2RampLog)
	}

	fmt.Println("\n================================================================================")
	fmt.Println("✓ HOÀN TẤT TOÀN DIỆN QUY TRÌNH 2 PHA ĐỊNH CHUẨN VÀ PROFILING!")
	fmt.Println("================================================================================")
	fmt.Printf("Báo cáo phân tích Sizing : %s\n", reportLog)
	if len(generatedSVGs) > 0 {
		for _, s := range generatedSVGs {
			fmt.Printf("  • FlameGraph SVG: %s\n", s)
		}
	} else {
		fmt.Println("  • FlameGraph SVG: (Không có file nào được tạo — xem cảnh báo phía trên)")
	}
	fmt.Println("================================================================================")
	return nil
}

func isValidSVGFile(path string) bool {
	if !fileExists(path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return false
	}
	return strings.Contains(string(data), "<svg")
}

// stopActiveLoadgen gửi tín hiệu SIGINT cho tiến trình capsizer-ramp đang chạy để dừng sớm an toàn và flush dữ liệu JSONL.
func (r *Runner) stopActiveLoadgen() {
	if r.cfg.LoadgenMode == "remote" && r.loadSSH != nil {
		_, _ = r.loadSSH.RunCommand(context.Background(), "pkill -INT -f 'capsizer-ramp' 2>/dev/null || true")
		return
	}
	_ = exec.Command("pkill", "-INT", "-f", "capsizer-ramp").Run()
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
		r.debugContainerName = "capsizer-debugger"
	case "sidecar":
		fmt.Printf("==> [K8s Target] Sử dụng Sidecar container '%s' trong Pod %s.\n", r.toolContainer(), r.cfg.K8sPod)
	case "cp":
		cName, _ := r.k8sClient.GetContainerName(ctx, r.cfg.K8sPod, r.cfg.K8sContainer)
		r.cfg.K8sContainer = cName
	}

	// Luôn biên dịch tĩnh và đồng bộ bản probe mới nhất (hỗ trợ đọc cgroup của app container qua /proc/<pid>/root/sys/fs/cgroup)
	// vào /tmp/probe của container công cụ để không phụ thuộc bản probe cũ đóng gói trong image sidecar.
	execContainer := r.toolContainer()
	fmt.Printf("==> [K8s Target] Đồng bộ binary probe mới nhất vào %s/%s (/tmp/probe)...\n", r.cfg.K8sPod, execContainer)
	probeBin, err := r.ensureLinuxBinary("probe", "./cmd/probe")
	if err != nil {
		if r.cfg.K8sMode == "cp" {
			return err
		}
		fmt.Printf("    [CẢNH BÁO] Không biên dịch được probe mới (%v), dùng bản có sẵn trong image.\n", err)
		return nil
	}
	if err := r.k8sClient.CopyToPod(ctx, probeBin, r.cfg.K8sPod, execContainer, "/tmp/probe"); err != nil {
		if r.cfg.K8sMode == "cp" {
			return fmt.Errorf("không thể chép probe vào Pod: %w", err)
		}
		fmt.Printf("    [CẢNH BÁO] Không thể cập nhật /tmp/probe (%v), dùng bản có sẵn trong image.\n", err)
		return nil
	}
	if out, err := r.k8sClient.ExecInPod(ctx, r.cfg.K8sPod, execContainer, "chmod +x /tmp/probe"); err != nil {
		if r.cfg.K8sMode == "cp" {
			return fmt.Errorf("không thể cấp quyền thực thi cho /tmp/probe: %w (%s)", err, out)
		}
	}
	return nil
}

// waitAppStable chờ ứng dụng mục tiêu trong Pod phản hồi /health/live HTTP 200 ổn định liên tiếp
// (tránh bắn tải hoặc profiling ngay khi Pod vừa báo Ready nhưng ứng dụng bên trong vẫn đang warm-up ngầm).
func (r *Runner) waitAppStable(ctx context.Context, execContainer string, requiredStreak int, timeout time.Duration) {
	if r.cfg.K8sPod == "" || requiredStreak <= 0 || timeout <= 0 {
		return
	}
	fmt.Printf("==> [Health Gate] Đang chờ ứng dụng (port %d) phản hồi /health/live 200 ổn định (%d lần liên tiếp, tối đa %ds)...\n",
		r.cfg.TargetPort, requiredStreak, int(timeout.Seconds()))

	checkCmd := fmt.Sprintf(
		"if [ -x /tmp/probe ]; then PBIN=/tmp/probe; elif command -v probe >/dev/null 2>&1; then PBIN=probe; else PBIN=/tmp/probe; fi; "+
			"$PBIN -port %d -target http://127.0.0.1:%d -health-timeout 3 -once 2>/dev/null",
		r.cfg.TargetPort, r.cfg.TargetPort,
	)

	deadline := time.Now().Add(timeout)
	streak := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		cCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		out, err := r.k8sClient.ExecInPod(cCtx, r.cfg.K8sPod, execContainer, checkCmd)
		cancel()

		compact := strings.Map(func(ru rune) rune {
			if ru == ' ' || ru == '\t' || ru == '\n' || ru == '\r' {
				return -1
			}
			return ru
		}, out)

		if err == nil && strings.Contains(compact, `"health_code":200`) {
			streak++
			if streak >= requiredStreak {
				fmt.Printf("    ✓ Ứng dụng đã ổn định (%d/%d lần /health/live = 200 OK).\n", streak, requiredStreak)
				return
			}
		} else {
			if streak > 0 {
				fmt.Printf("    [!] /health/live nhấp nháy, reset streak (%d -> 0)...\n", streak)
			}
			streak = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	fmt.Printf("    [CẢNH BÁO] Hết thời gian chờ %ds nhưng ứng dụng chưa đạt %d lần /health/live 200 liên tiếp (streak: %d/%d). Tiếp tục chạy...\n",
		int(timeout.Seconds()), requiredStreak, streak, requiredStreak)
}

// warmupApplication bắn một loạt request thật từ kịch bản ở tần suất thấp (0.5 req/s trong 10s, tối đa 2 vòng)
// để kích hoạt trước các tác vụ khởi tạo lười (lazy-init như giải nén model langid/bz2, nạp từ điển BM25, kết nối DB)
// cho đến khi độ trễ TTFT ổn định trước khi bước vào đo đạc chính thức hoặc chạy Profiler.
func (r *Runner) warmupApplication(ctx context.Context, execContainer string) {
	if ctx.Err() != nil {
		return
	}
	fmt.Println("==> [Warm-up Nghiệp vụ] Đang bắn request mồi thật (0.5 req/s) để khởi tạo sẵn model/cache (tránh nhiễu cold-start như langid/BM25)...")
	warmupLog := filepath.Join(r.cfg.OutDir, "warmup-ramp.jsonl")

	for round := 1; round <= 2; round++ {
		if ctx.Err() != nil {
			return
		}
		_ = r.runLoadgen(ctx, "0.5", 10, 0, 0, warmupLog, true)
		if r.cfg.LoadgenMode == "remote" && r.loadSSH != nil {
			remoteLog := fmt.Sprintf("%s/current-ramp.jsonl", r.cfg.LoadgenDir)
			_ = r.loadSSH.DownloadFile(ctx, remoteLog, warmupLog)
		}

		recs, err := analyzer.LoadRamp(warmupLog)
		if err != nil {
			break
		}

		var ttfts []float64
		for _, rec := range recs {
			if rec.Type != "run_meta" && rec.Ok && rec.TtftMs != nil {
				ttfts = append(ttfts, *rec.TtftMs)
			}
		}

		if len(ttfts) >= 2 {
			firstTTFT := ttfts[0]
			lastTTFT := ttfts[len(ttfts)-1]
			prevTTFT := ttfts[len(ttfts)-2]
			if (lastTTFT < 2000 && prevTTFT < 2000) || (firstTTFT > 3000 && lastTTFT < firstTTFT/3) || round == 2 {
				fmt.Printf("    ✓ Warm-up hoàn tất (vòng %d, %d req OK: TTFT đầu=%.0fms -> cuối=%.0fms).\n",
					round, len(ttfts), firstTTFT, lastTTFT)
				break
			}
			fmt.Printf("    • Vòng warm-up %d: ứng dụng vừa nạp model (TTFT đầu=%.0fms -> cuối=%.0fms), chạy thêm 1 vòng ngắn để ổn định hẳn...\n",
				round, firstTTFT, lastTTFT)
		} else if round < 2 {
			fmt.Printf("    • Vòng warm-up %d: mới hoàn tất %d request (model đang khởi tạo), chạy thêm 1 vòng ngắn...\n",
				round, len(ttfts))
		}
	}

	// Chờ 3s để CPU/queue hạ nhiệt hoàn toàn sau warm-up và xác nhận lại /health/live = 200
	time.Sleep(3 * time.Second)
	if r.cfg.K8sPod != "" && execContainer != "" {
		r.waitAppStable(ctx, execContainer, 2, 30*time.Second)
	}
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
		// Luôn cố gắng tải remoteLog về ngay cả khi tiến trình dừng sớm để không mất dữ liệu
		if dlErr := r.loadSSH.DownloadFile(context.Background(), remoteLog, outPath); dlErr != nil && err == nil {
			return fmt.Errorf("không thể tải file kết quả ramp từ máy phát tải: %w", dlErr)
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
