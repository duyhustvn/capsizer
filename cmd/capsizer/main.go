package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"capsizer/pkg/orchestrator"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := orchestrator.NewDefaultConfig()

	// Tự động nạp .env.remote nếu tồn tại
	if _, err := os.Stat(".env.remote"); err == nil {
		cfg.ApplyInfraEnv(".env.remote")
	}

	switch command {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		bindFlags(fs, cfg)
		_ = fs.Parse(os.Args[2:])

		if cfg.Scenario != "" {
			_ = cfg.LoadScenarioPackage(cfg.Scenario)
		}
		if cfg.ScenarioEnv != "" {
			cfg.ApplyScenarioEnv(cfg.ScenarioEnv)
		}

		runner, err := orchestrator.NewRunner(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}

		if err := runner.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Thực thi thất bại: %v\n", err)
			os.Exit(1)
		}

	case "check":
		cfg.OnlyCheck = true
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		bindFlags(fs, cfg)
		_ = fs.Parse(os.Args[2:])

		if cfg.Scenario != "" {
			_ = cfg.LoadScenarioPackage(cfg.Scenario)
		}

		runner, err := orchestrator.NewRunner(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}

		if err := runner.PreflightCheck(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Kiểm tra liên kết thất bại: %v\n", err)
			os.Exit(1)
		}

	case "batch":
		fs := flag.NewFlagSet("batch", flag.ExitOnError)
		bindFlags(fs, cfg)
		_ = fs.Parse(os.Args[2:])
		scenarios := fs.Args()

		if err := orchestrator.RunBatch(ctx, cfg, scenarios); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Batch runner thất bại: %v\n", err)
			os.Exit(1)
		}

	default:
		// Tương thích ngược: nếu gõ thẳng `capsizer --scenario chatbot` mà không có subcommand
		fs := flag.NewFlagSet("capsizer", flag.ExitOnError)
		bindFlags(fs, cfg)
		_ = fs.Parse(os.Args[1:])

		if cfg.Scenario != "" {
			_ = cfg.LoadScenarioPackage(cfg.Scenario)
		}

		runner, err := orchestrator.NewRunner(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}

		if err := runner.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}
	}
}

func bindFlags(fs *flag.FlagSet, cfg *orchestrator.Config) {
	fs.StringVar(&cfg.Scenario, "scenario", cfg.Scenario, "Tên kịch bản (ví dụ: chatbot, sample-get)")
	fs.StringVar(&cfg.ScenarioEnv, "scenario-env", cfg.ScenarioEnv, "Đường dẫn file scenario.env độc lập")
	fs.StringVar(&cfg.URL, "url", cfg.URL, "URL endpoint nhận tải")
	fs.StringVar(&cfg.Steps, "steps", cfg.Steps, "Dải bậc tải RPS (ví dụ: 2,4,8,16,32)")
	fs.IntVar(&cfg.StepSeconds, "step-seconds", cfg.StepSeconds, "Thời gian mỗi bậc tải (giây)")
	fs.IntVar(&cfg.BaselineWait, "baseline-wait", cfg.BaselineWait, "Thời gian chờ đo CPU/RAM nền (giây)")
	fs.IntVar(&cfg.Cooldown, "cooldown", cfg.Cooldown, "Thời gian nghỉ giữa các bậc (giây)")
	fs.StringVar(&cfg.SetPodCPU, "set-pod-cpu", cfg.SetPodCPU, "Tự động đưa CPU Pod về số core chỉ định (ví dụ: 1 hoặc 1000m)")
	fs.BoolVar(&cfg.RestorePodCPU, "restore-pod-cpu", cfg.RestorePodCPU, "Tự động khôi phục CPU ban đầu sau khi kết thúc")
	fs.StringVar(&cfg.ProfileType, "profile-type", cfg.ProfileType, "Công cụ profiling: pyspy, perf, all")
	fs.StringVar(&cfg.OutDir, "out-dir", cfg.OutDir, "Thư mục lưu trữ artifact kết quả")
	fs.BoolVar(&cfg.OnlyCheck, "check", cfg.OnlyCheck, "Kiểm tra kết nối 3 máy rồi thoát")

	// Hạ tầng
	fs.StringVar(&cfg.LoadgenMode, "loadgen-mode", cfg.LoadgenMode, "Chế độ phát tải: local hoặc remote")
	fs.StringVar(&cfg.LoadgenHost, "loadgen-host", cfg.LoadgenHost, "Host máy phát tải")
	fs.StringVar(&cfg.K8sSSHHost, "k8s-ssh-host", cfg.K8sSSHHost, "Host server cụm K8s")
	fs.StringVar(&cfg.K8sNamespace, "namespace", cfg.K8sNamespace, "K8s namespace")
	fs.StringVar(&cfg.K8sSelector, "selector", cfg.K8sSelector, "K8s pod selector")
	fs.StringVar(&cfg.K8sPod, "pod", cfg.K8sPod, "Tên Pod mục tiêu")
	fs.StringVar(&cfg.K8sContainer, "container", cfg.K8sContainer, "Tên container ứng dụng")
}

func printUsage() {
	fmt.Println(`Sử dụng: capsizer <command> [OPTIONS]

Lệnh hỗ trợ:
  run     Chạy quy trình kiểm thử tải 2 Pha (Sizing + Profiling)
  check   Kiểm tra kết nối liên kết 3 máy (Skill -> LoadGen -> K8s)
  batch   Chạy tự động hàng loạt nhiều kịch bản (Multi-Scenario Batch)

Ví dụ:
  capsizer run --scenario chatbot
  capsizer run --scenario chatbot --set-pod-cpu 1
  capsizer check --scenario chatbot
  capsizer batch chatbot sample-post sample-get`)
}
