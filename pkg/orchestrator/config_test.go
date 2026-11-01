package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile_StripsInlineComments(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env.remote")

	content := `
# Comment đầu dòng
LOADGEN_MODE="remote"                  # 'remote' (khuyên dùng) hoặc 'local'
REMOTE_HOST="192.168.1.50"             # IP hoặc hostname máy phát tải
REMOTE_PORT=2222                       # Cổng SSH không bọc nháy
REMOTE_KEY=""                          # Để trống nếu dùng key mặc định
SINGLE_QUOTED='app=chatbot'            # Nháy đơn kèm comment
export EXPORTED_VAR="hello"            # Có tiền tố export
URL_WITH_HASH=http://10.0.0.1:8080/p#v # URL có dấu # sát ký tự và comment cách khoảng trắng
`
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp env file: %v", err)
	}

	envMap, err := LoadEnvFile(envFile)
	if err != nil {
		t.Fatalf("LoadEnvFile returned error: %v", err)
	}

	expected := map[string]string{
		"LOADGEN_MODE":  "remote",
		"REMOTE_HOST":   "192.168.1.50",
		"REMOTE_PORT":   "2222",
		"REMOTE_KEY":    "",
		"SINGLE_QUOTED": "app=chatbot",
		"EXPORTED_VAR":  "hello",
		"URL_WITH_HASH": "http://10.0.0.1:8080/p#v",
	}

	for k, want := range expected {
		if got := envMap[k]; got != want {
			t.Errorf("envMap[%q] = %q; want %q", k, got, want)
		}
	}
}

func TestApplyInfraEnvAndScenarioPackage(t *testing.T) {
	cfg := NewDefaultConfig()
	examplePath := filepath.Join(cfg.ProjectRoot, ".env.remote.example")
	if !fileExists(examplePath) {
		t.Skip(".env.remote.example not found")
	}

	cfg.ApplyInfraEnv(examplePath)

	if cfg.LoadgenMode != "remote" {
		t.Errorf("LoadgenMode = %q; want %q", cfg.LoadgenMode, "remote")
	}
	if cfg.LoadgenHost != "192.168.1.50" {
		t.Errorf("LoadgenHost = %q; want %q", cfg.LoadgenHost, "192.168.1.50")
	}
	if cfg.LoadgenPort != 22 {
		t.Errorf("LoadgenPort = %d; want 22", cfg.LoadgenPort)
	}
	if cfg.LoadgenKey != "" {
		t.Errorf("LoadgenKey = %q; want empty string", cfg.LoadgenKey)
	}
	if cfg.LoadgenDir != "~/capsizer-loadtest" {
		t.Errorf("LoadgenDir = %q; want %q", cfg.LoadgenDir, "~/capsizer-loadtest")
	}
	if cfg.K8sSSHHost != "192.168.1.100" {
		t.Errorf("K8sSSHHost = %q; want %q", cfg.K8sSSHHost, "192.168.1.100")
	}
	if !cfg.K8sUseSudo {
		t.Errorf("K8sUseSudo = false; want true")
	}
	if cfg.K8sMode != "debug" {
		t.Errorf("K8sMode = %q; want %q", cfg.K8sMode, "debug")
	}

	// Nạp scenario chatbot
	if err := cfg.LoadScenarioPackage("chatbot"); err != nil {
		t.Fatalf("LoadScenarioPackage(chatbot) error: %v", err)
	}
	cfg.Finalize()

	if cfg.K8sSelector != "app=chatbot" {
		t.Errorf("K8sSelector = %q; want %q", cfg.K8sSelector, "app=chatbot")
	}
	if cfg.SetPodCPU != "1" {
		t.Errorf("SetPodCPU = %q; want %q", cfg.SetPodCPU, "1")
	}
	if cfg.ProfileType != "pyspy" {
		t.Errorf("ProfileType = %q; want %q", cfg.ProfileType, "pyspy")
	}
	if cfg.URL != "http://10.0.0.10:8000/chat" {
		t.Errorf("URL = %q; want %q", cfg.URL, "http://10.0.0.10:8000/chat")
	}
	if cfg.TargetPort != 8000 {
		t.Errorf("TargetPort = %d; want 8000", cfg.TargetPort)
	}
}

func TestExtractPortFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"http://10.43.92.185:8000/v1/chat", 8000},
		{"http://10.43.92.185:9090/api", 9090},
		{"https://api.example.com/v1/chat", 443},
		{"http://api.example.com/v1/chat", 80},
	}
	for _, tc := range cases {
		if got := extractPortFromURL(tc.url); got != tc.want {
			t.Errorf("extractPortFromURL(%q) = %d; want %d", tc.url, got, tc.want)
		}
	}
}

func TestSSHClient_RespectsSSHConfigWhenUserAndPortEmpty(t *testing.T) {
	client, err := NewSSHClient("", "k8s-prod-alias", 0, "")
	if err != nil {
		t.Fatalf("NewSSHClient error: %v", err)
	}
	args := client.sshArgs("kubectl get pods")
	for _, a := range args {
		if a == "-p" || a == "-l" || a == "-i" {
			t.Errorf("expected sshArgs not to override ~/.ssh/config with %q, got args: %v", a, args)
		}
	}
	if client.DisplayTarget() != "k8s-prod-alias" {
		t.Errorf("DisplayTarget() = %q; want 'k8s-prod-alias'", client.DisplayTarget())
	}
}
