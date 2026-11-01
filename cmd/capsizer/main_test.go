package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCommandConfig_PrecedenceAndCommentStripping(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env.remote")

	envContent := `
LOADGEN_MODE="remote"              # 'remote' (khuyên dùng) hoặc 'local'
REMOTE_HOST="192.168.1.50"         # IP máy phát tải
REMOTE_PORT="2222"                 # Cổng SSH
K8S_SSH_HOST="192.168.1.100"       # IP K8s
K8S_USE_SUDO="true"                # Dùng sudo
K8S_NAMESPACE="prod-ns"            # Namespace mặc định
`
	if err := os.WriteFile(envFile, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to write env file: %v", err)
	}

	// 1. Kiểm tra nạp .env.remote + scenario chatbot mà không truyền cờ ghi đè
	cfg, _, err := parseCommandConfig("run", []string{
		"--env-file", envFile,
		"--scenario", "chatbot",
	}, true)
	if err != nil {
		t.Fatalf("parseCommandConfig error: %v", err)
	}

	if cfg.LoadgenMode != "remote" {
		t.Errorf("LoadgenMode = %q; want 'remote'", cfg.LoadgenMode)
	}
	if cfg.LoadgenHost != "192.168.1.50" {
		t.Errorf("LoadgenHost = %q; want '192.168.1.50'", cfg.LoadgenHost)
	}
	if cfg.LoadgenPort != 2222 {
		t.Errorf("LoadgenPort = %d; want 2222", cfg.LoadgenPort)
	}
	if cfg.K8sSSHHost != "192.168.1.100" {
		t.Errorf("K8sSSHHost = %q; want '192.168.1.100'", cfg.K8sSSHHost)
	}
	if cfg.K8sNamespace != "prod-ns" {
		t.Errorf("K8sNamespace = %q; want 'prod-ns'", cfg.K8sNamespace)
	}
	if cfg.K8sSelector != "app=chatbot" {
		t.Errorf("K8sSelector = %q; want 'app=chatbot'", cfg.K8sSelector)
	}
	if cfg.Steps != "2,4,8,16,32" {
		t.Errorf("Steps = %q; want '2,4,8,16,32'", cfg.Steps)
	}
	if cfg.ProfileType != "pyspy" {
		t.Errorf("ProfileType = %q; want 'pyspy'", cfg.ProfileType)
	}

	// 2. Kiểm tra cờ CLI ghi đè cả .env.remote và scenario.env (dù đặt trước hay sau --scenario)
	cfg2, _, err := parseCommandConfig("run", []string{
		"--steps", "10,20,40",
		"--set-pod-cpu", "2",
		"--loadgen-mode", "local",
		"--env-file", envFile,
		"--scenario", "chatbot",
		"--profile-type", "perf",
		"--port", "9090",
		"--no-restore-cpu",
		"--k8s-no-sudo",
	}, true)
	if err != nil {
		t.Fatalf("parseCommandConfig override error: %v", err)
	}

	if cfg2.Steps != "10,20,40" {
		t.Errorf("CLI override Steps = %q; want '10,20,40'", cfg2.Steps)
	}
	if cfg2.SetPodCPU != "2" {
		t.Errorf("CLI override SetPodCPU = %q; want '2'", cfg2.SetPodCPU)
	}
	if cfg2.LoadgenMode != "local" {
		t.Errorf("CLI override LoadgenMode = %q; want 'local'", cfg2.LoadgenMode)
	}
	if cfg2.ProfileType != "perf" {
		t.Errorf("CLI override ProfileType = %q; want 'perf'", cfg2.ProfileType)
	}
	if cfg2.TargetPort != 9090 {
		t.Errorf("CLI override TargetPort = %d; want 9090", cfg2.TargetPort)
	}
	if cfg2.RestorePodCPU {
		t.Errorf("CLI override RestorePodCPU = true; want false (--no-restore-cpu)")
	}
	if cfg2.K8sUseSudo {
		t.Errorf("CLI override K8sUseSudo = true; want false (--k8s-no-sudo)")
	}
}
