package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"capsizer/pkg/orchestrator"
)

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(val string) error {
	*m = append(*m, val)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	if command == "-h" || command == "--help" || command == "help" {
		printUsage()
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch command {
	case "run":
		cfg, _, err := parseCommandConfig("run", os.Args[2:], true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
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
		cfg, _, err := parseCommandConfig("check", os.Args[2:], true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}
		cfg.OnlyCheck = true

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
		rawArgs := os.Args[2:]
		cfg, restArgs, err := parseCommandConfig("batch", rawArgs, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
		}

		// Hàm áp dụng lại các cờ CLI ưu tiên cao nhất lên từng kịch bản trong batch
		applyCLIOverrides := func(targetCfg *orchestrator.Config) {
			fs := flag.NewFlagSet("batch-override", flag.ContinueOnError)
			bindFlags(fs, targetCfg)
			_ = fs.Parse(rawArgs)
			targetCfg.Finalize()
		}

		if err := orchestrator.RunBatchWithOverrides(ctx, cfg, restArgs, applyCLIOverrides); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Batch runner thất bại: %v\n", err)
			os.Exit(1)
		}

	default:
		// Tương thích ngược: nếu gõ thẳng `capsizer --scenario chatbot` mà không có subcommand
		cfg, _, err := parseCommandConfig("capsizer", os.Args[1:], true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			os.Exit(1)
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

// parseCommandConfig nạp cấu hình theo đúng thứ tự ưu tiên:
// 1. Mặc định (NewDefaultConfig)
// 2. Hạ tầng (.env.remote / --env-file)
// 3. Kịch bản (scenario.json + scenario.env từ --scenario / --scenario-env)
// 4. Cờ CLI trực tiếp (Explicit CLI Flags)
func parseCommandConfig(cmdName string, args []string, loadScenario bool) (*orchestrator.Config, []string, error) {
	cfg := orchestrator.NewDefaultConfig()

	envFile, scenarioName, scenarioEnvPath, isHelp := preScanArgs(args)

	if err := cfg.ResolveAndApplyInfraEnv(envFile); err != nil && !isHelp {
		return nil, nil, err
	}

	if loadScenario {
		if scenarioName != "" {
			if err := cfg.LoadScenarioPackage(scenarioName); err != nil && !isHelp {
				return nil, nil, err
			}
		}
		if scenarioEnvPath != "" {
			if !fileExists(scenarioEnvPath) && !isHelp {
				return nil, nil, fmt.Errorf("không tìm thấy file scenario-env: %s", scenarioEnvPath)
			}
			cfg.ScenarioEnv = scenarioEnvPath
			cfg.ApplyScenarioEnv(scenarioEnvPath)
		}
	}

	fs := flag.NewFlagSet(cmdName, flag.ExitOnError)
	noRestoreCPU := false
	k8sNoSudo := false
	bindFlagsWithHelpers(fs, cfg, &noRestoreCPU, &k8sNoSudo)
	_ = fs.Parse(args)

	// Kiểm tra xem --port có được truyền tường minh trên CLI không
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			cfg.PortExplicit = true
		}
	})

	if noRestoreCPU {
		cfg.RestorePodCPU = false
	}
	if k8sNoSudo {
		cfg.K8sUseSudo = false
	}

	cfg.Finalize()
	return cfg, fs.Args(), nil
}

func preScanArgs(args []string) (envFile, scenario, scenarioEnv string, isHelp bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" || a == "-help" {
			isHelp = true
			continue
		}
		if val, ok := extractFlagValue(args, &i, "--env-file", "-env-file"); ok {
			envFile = val
			continue
		}
		if val, ok := extractFlagValue(args, &i, "--scenario", "-scenario"); ok {
			scenario = val
			continue
		}
		if val, ok := extractFlagValue(args, &i, "--scenario-env", "-scenario-env"); ok {
			scenarioEnv = val
			continue
		}
	}
	return
}

func extractFlagValue(args []string, idx *int, names ...string) (string, bool) {
	arg := args[*idx]
	for _, name := range names {
		if arg == name {
			if *idx+1 < len(args) {
				*idx++
				return args[*idx], true
			}
			return "", true
		}
		prefix := name + "="
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}
	return "", false
}

func bindFlags(fs *flag.FlagSet, cfg *orchestrator.Config) {
	noRestore := false
	noSudo := false
	bindFlagsWithHelpers(fs, cfg, &noRestore, &noSudo)
}

func bindFlagsWithHelpers(fs *flag.FlagSet, cfg *orchestrator.Config, noRestoreCPU, k8sNoSudo *bool) {
	// Kịch bản & Phát tải
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "Đường dẫn file cấu hình hạ tầng (.env.remote)")
	fs.StringVar(&cfg.Scenario, "scenario", cfg.Scenario, "Tên kịch bản trong scenarios/ (ví dụ: chatbot, sample-get)")
	fs.StringVar(&cfg.ScenarioEnv, "scenario-env", cfg.ScenarioEnv, "Đường dẫn file scenario.env độc lập")
	fs.StringVar(&cfg.URL, "url", cfg.URL, "URL endpoint nhận tải")
	fs.StringVar(&cfg.Method, "method", cfg.Method, "HTTP method (GET, POST, ...)")
	fs.StringVar(&cfg.Body, "body", cfg.Body, "Payload body trực tiếp")
	fs.StringVar(&cfg.BodyFile, "body-file", cfg.BodyFile, "Đường dẫn file chứa payload body")
	fs.StringVar(&cfg.Steps, "steps", cfg.Steps, "Dải bậc tải RPS (ví dụ: 2,4,8,16,32)")
	fs.IntVar(&cfg.StepSeconds, "step-seconds", cfg.StepSeconds, "Thời gian mỗi bậc tải (giây)")
	fs.IntVar(&cfg.BaselineWait, "baseline-wait", cfg.BaselineWait, "Thời gian chờ đo CPU/RAM nền (giây)")
	fs.IntVar(&cfg.Cooldown, "cooldown", cfg.Cooldown, "Thời gian nghỉ giữa các bậc (giây)")
	fs.IntVar(&cfg.Cooldown, "settle", cfg.Cooldown, "Thời gian nghỉ giữa các bậc (alias của --cooldown)")
	fs.IntVar(&cfg.Timeout, "timeout", cfg.Timeout, "HTTP request timeout (giây)")
	fs.IntVar(&cfg.MaxInflight, "max-inflight", cfg.MaxInflight, "Số request đồng thời tối đa")
	fs.BoolVar(&cfg.CacheBust, "cache-bust", cfg.CacheBust, "Chống cache bằng query ngẫu nhiên")
	fs.BoolVar(&cfg.SkipVerify, "skip-verify", cfg.SkipVerify, "Bỏ qua kiểm tra chứng chỉ TLS/SSL")
	fs.Var((*multiStringFlag)(&cfg.ExtraOpts), "opt", "Tùy chọn bổ sung key=value cho kịch bản (có thể truyền nhiều lần)")

	// Điều khiển CPU & Profiling
	fs.StringVar(&cfg.SetPodCPU, "set-pod-cpu", cfg.SetPodCPU, "Tự động đưa CPU Pod về số core chỉ định (ví dụ: 1 hoặc 1000m)")
	fs.BoolVar(&cfg.RestorePodCPU, "restore-pod-cpu", cfg.RestorePodCPU, "Tự động khôi phục CPU ban đầu sau khi kết thúc")
	fs.BoolVar(noRestoreCPU, "no-restore-cpu", false, "Giữ nguyên mức CPU mới sau khi đo xong (không hoàn nguyên)")
	fs.StringVar(&cfg.ProfileType, "profile-type", cfg.ProfileType, "Công cụ profiling: pyspy, perf, all")
	fs.IntVar(&cfg.TargetPort, "port", cfg.TargetPort, "Cổng ứng dụng bên trong Pod (mặc định tự bóc từ URL)")
	fs.IntVar(&cfg.TargetPID, "pid", cfg.TargetPID, "PID tiến trình mục tiêu trong Pod (0 = tự động dò tìm)")
	fs.StringVar(&cfg.OutDir, "out-dir", cfg.OutDir, "Thư mục lưu trữ artifact kết quả")
	fs.BoolVar(&cfg.OnlyCheck, "check", cfg.OnlyCheck, "Kiểm tra kết nối 3 máy rồi thoát")

	// Hạ tầng Machine 2 (Load Generator)
	fs.StringVar(&cfg.LoadgenMode, "loadgen-mode", cfg.LoadgenMode, "Chế độ phát tải: local hoặc remote")
	fs.StringVar(&cfg.LoadgenHost, "loadgen-host", cfg.LoadgenHost, "Host máy phát tải")
	fs.StringVar(&cfg.LoadgenHost, "remote-host", cfg.LoadgenHost, "Host máy phát tải (alias của --loadgen-host)")
	fs.StringVar(&cfg.LoadgenUser, "loadgen-user", cfg.LoadgenUser, "SSH user máy phát tải")
	fs.StringVar(&cfg.LoadgenUser, "remote-user", cfg.LoadgenUser, "SSH user máy phát tải (alias của --loadgen-user)")
	fs.IntVar(&cfg.LoadgenPort, "loadgen-port", cfg.LoadgenPort, "Cổng SSH máy phát tải")
	fs.IntVar(&cfg.LoadgenPort, "remote-port", cfg.LoadgenPort, "Cổng SSH máy phát tải (alias của --loadgen-port)")
	fs.StringVar(&cfg.LoadgenKey, "loadgen-key", cfg.LoadgenKey, "Đường dẫn SSH key máy phát tải")
	fs.StringVar(&cfg.LoadgenKey, "remote-key", cfg.LoadgenKey, "Đường dẫn SSH key máy phát tải (alias của --loadgen-key)")
	fs.StringVar(&cfg.LoadgenJump, "loadgen-jump", cfg.LoadgenJump, "Jump host (ProxyJump) tới máy phát tải (nếu không cấu hình trong ~/.ssh/config)")
	fs.StringVar(&cfg.LoadgenJump, "remote-jump", cfg.LoadgenJump, "Jump host tới máy phát tải (alias của --loadgen-jump)")
	fs.StringVar(&cfg.LoadgenDir, "loadgen-dir", cfg.LoadgenDir, "Thư mục làm việc trên máy phát tải")
	fs.StringVar(&cfg.LoadgenDir, "remote-dir", cfg.LoadgenDir, "Thư mục làm việc trên máy phát tải (alias của --loadgen-dir)")

	// Hạ tầng Machine 3 (Server cụm K8s)
	fs.StringVar(&cfg.K8sSSHHost, "k8s-ssh-host", cfg.K8sSSHHost, "Host hoặc Alias trong ~/.ssh/config của server cụm K8s")
	fs.StringVar(&cfg.K8sSSHUser, "k8s-ssh-user", cfg.K8sSSHUser, "SSH user server cụm K8s (để trống nếu dùng ~/.ssh/config)")
	fs.IntVar(&cfg.K8sSSHPort, "k8s-ssh-port", cfg.K8sSSHPort, "Cổng SSH server cụm K8s (0 = dùng ~/.ssh/config hoặc 22)")
	fs.StringVar(&cfg.K8sSSHKey, "k8s-ssh-key", cfg.K8sSSHKey, "Đường dẫn SSH key server cụm K8s (để trống nếu dùng ~/.ssh/config)")
	fs.StringVar(&cfg.K8sSSHJump, "k8s-ssh-jump", cfg.K8sSSHJump, "Jump host (ProxyJump) tới server K8s (nếu không khai báo trong ~/.ssh/config)")
	fs.BoolVar(&cfg.K8sUseSudo, "k8s-use-sudo", cfg.K8sUseSudo, "Sử dụng sudo khi gọi kubectl trên server K8s")
	fs.BoolVar(k8sNoSudo, "k8s-no-sudo", false, "Không dùng sudo khi gọi kubectl trên server K8s")
	fs.StringVar(&cfg.K8sNamespace, "namespace", cfg.K8sNamespace, "K8s namespace")
	fs.StringVar(&cfg.K8sSelector, "selector", cfg.K8sSelector, "K8s pod label selector")
	fs.StringVar(&cfg.K8sPod, "pod", cfg.K8sPod, "Tên Pod mục tiêu")
	fs.StringVar(&cfg.K8sContainer, "container", cfg.K8sContainer, "Tên container ứng dụng")
	fs.StringVar(&cfg.K8sMode, "k8s-mode", cfg.K8sMode, "Chế độ công cụ K8s: debug, sidecar, cp")
	fs.StringVar(&cfg.SidecarImage, "sidecar-image", cfg.SidecarImage, "Image chứa công cụ probe/profiler")
	fs.StringVar(&cfg.SidecarContainer, "sidecar-container", cfg.SidecarContainer, "Tên container sidecar")
	fs.StringVar(&cfg.KubeconfigPath, "kubeconfig", cfg.KubeconfigPath, "Đường dẫn file kubeconfig")
	fs.StringVar(&cfg.ContextName, "context", cfg.ContextName, "Tên K8s context")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
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
