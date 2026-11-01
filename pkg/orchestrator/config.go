package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config chứa toàn bộ thông số hạ tầng 3 máy và kịch bản thực thi.
type Config struct {
	// [Machine 1] Skill Local
	OutDir    string
	OnlyCheck bool

	// [Machine 2] Máy phát tải (Load Generator)
	LoadgenMode string // "local" hoặc "remote"
	LoadgenHost string
	LoadgenUser string
	LoadgenPort int
	LoadgenKey  string
	LoadgenDir  string

	// [Machine 3] Server Cụm K8s
	K8sSSHHost         string
	K8sSSHUser         string
	K8sSSHPort         int
	K8sSSHKey          string
	K8sUseSudo         bool
	K8sNamespace       string
	K8sPod             string
	K8sSelector        string
	K8sContainer       string
	K8sMode            string // "debug", "sidecar", "cp"
	SidecarImage       string
	SidecarContainer   string
	SetPodCPU          string // ví dụ "1" hoặc "1000m"
	RestorePodCPU      bool   // mặc định true
	TargetPort         int
	KubeconfigPath     string
	ContextName        string

	// [Kịch bản]
	Scenario     string
	ScenarioDir  string
	ScenarioJSON string
	ScenarioEnv  string
	URL          string
	Method       string
	Body         string
	BodyFile     string
	Steps        string
	StepSeconds  int
	BaselineWait int
	Cooldown     int
	Timeout      int
	MaxInflight  int
	CacheBust    bool
	SkipVerify   bool
	ProfileType  string // "pyspy", "perf", "all", ...
	ExtraOpts    []string
}

// NewDefaultConfig khởi tạo cấu hình với các giá trị mặc định.
func NewDefaultConfig() *Config {
	return &Config{
		LoadgenMode:      "local",
		LoadgenUser:      "vbox",
		LoadgenPort:      22,
		LoadgenDir:       "~/capsizer-loadtest",
		K8sSSHUser:       "vbox",
		K8sSSHPort:       22,
		K8sUseSudo:       true,
		K8sNamespace:     "default",
		K8sMode:          "debug",
		SidecarImage:     "docker.io/duyle95/profiling-sidecar:latest",
		SidecarContainer: "sidecar",
		RestorePodCPU:    true,
		TargetPort:       8000,
		Method:           "GET",
		Steps:            "2,4,8,16,32",
		StepSeconds:      30,
		BaselineWait:     60,
		Cooldown:         15,
		Timeout:          120,
		MaxInflight:      2000,
		ProfileType:      "pyspy",
	}
}

// LoadEnvFile đọc một file .env và trả về map key-value.
func LoadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	res := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			res[k] = v
		}
	}
	return res, scanner.Err()
}

// ApplyInfraEnv áp dụng cấu hình từ .env.remote.
func (c *Config) ApplyInfraEnv(path string) {
	envMap, err := LoadEnvFile(path)
	if err != nil {
		return
	}

	if v, ok := envMap["LOADGEN_MODE"]; ok && v != "" { c.LoadgenMode = v }
	if v, ok := envMap["REMOTE_HOST"]; ok && v != "" { c.LoadgenHost = v }
	if v, ok := envMap["REMOTE_USER"]; ok && v != "" { c.LoadgenUser = v }
	if v, ok := envMap["REMOTE_PORT"]; ok && v != "" { c.LoadgenPort, _ = strconv.Atoi(v) }
	if v, ok := envMap["REMOTE_KEY"]; ok && v != "" { c.LoadgenKey = v }
	if v, ok := envMap["REMOTE_DIR"]; ok && v != "" { c.LoadgenDir = v }

	if v, ok := envMap["K8S_SSH_HOST"]; ok && v != "" { c.K8sSSHHost = v }
	if v, ok := envMap["K8S_SSH_USER"]; ok && v != "" { c.K8sSSHUser = v }
	if v, ok := envMap["K8S_SSH_PORT"]; ok && v != "" { c.K8sSSHPort, _ = strconv.Atoi(v) }
	if v, ok := envMap["K8S_SSH_KEY"]; ok && v != "" { c.K8sSSHKey = v }
	if v, ok := envMap["K8S_USE_SUDO"]; ok && v != "" { c.K8sUseSudo = (v == "true" || v == "1") }
	if v, ok := envMap["K8S_NAMESPACE"]; ok && v != "" { c.K8sNamespace = v }
}

// ApplyScenarioEnv áp dụng cấu hình từ scenario.env.
func (c *Config) ApplyScenarioEnv(path string) {
	envMap, err := LoadEnvFile(path)
	if err != nil {
		return
	}

	if v, ok := envMap["K8S_SELECTOR"]; ok && v != "" { c.K8sSelector = v }
	if v, ok := envMap["SET_POD_CPU"]; ok && v != "" { c.SetPodCPU = v }
	if v, ok := envMap["RESTORE_POD_CPU"]; ok && v != "" { c.RestorePodCPU = (v != "false" && v != "0") }
	if v, ok := envMap["PROFILE_TYPE"]; ok && v != "" { c.ProfileType = v }
	if v, ok := envMap["TARGET_PORT"]; ok && v != "" { c.TargetPort, _ = strconv.Atoi(v) }
	if v, ok := envMap["STEPS"]; ok && v != "" { c.Steps = v }
	if v, ok := envMap["STEP_SECONDS"]; ok && v != "" { c.StepSeconds, _ = strconv.Atoi(v) }
	if v, ok := envMap["BASELINE_WAIT"]; ok && v != "" { c.BaselineWait, _ = strconv.Atoi(v) }
	if v, ok := envMap["COOLDOWN"]; ok && v != "" { c.Cooldown, _ = strconv.Atoi(v) }
	if v, ok := envMap["TIMEOUT"]; ok && v != "" { c.Timeout, _ = strconv.Atoi(v) }
	if v, ok := envMap["MAX_INFLIGHT"]; ok && v != "" { c.MaxInflight, _ = strconv.Atoi(v) }
	if v, ok := envMap["CACHE_BUST"]; ok && v != "" { c.CacheBust = (v == "true" || v == "1") }
	if v, ok := envMap["URL"]; ok && v != "" { c.URL = v }
	if v, ok := envMap["METHOD"]; ok && v != "" { c.Method = v }
	if v, ok := envMap["BODY"]; ok && v != "" { c.Body = v }
	if v, ok := envMap["BODY_FILE"]; ok && v != "" {
		if !filepath.IsAbs(v) {
			v = filepath.Join(filepath.Dir(path), v)
		}
		c.BodyFile = v
	}
}

// LoadScenarioPackage tự động tìm và nạp kịch bản theo tên thư mục.
func (c *Config) LoadScenarioPackage(name string) error {
	dir := filepath.Join("scenarios", name)
	if !dirExists(dir) {
		if dirExists(name) {
			dir = name
		} else {
			return fmt.Errorf("không tìm thấy thư mục kịch bản: %s", name)
		}
	}

	c.Scenario = filepath.Base(dir)
	c.ScenarioDir = dir

	envPath := filepath.Join(dir, "scenario.env")
	if !fileExists(envPath) {
		envPath = filepath.Join(dir, ".env")
	}
	if fileExists(envPath) {
		c.ScenarioEnv = envPath
		c.ApplyScenarioEnv(envPath)
	}

	jsonPath := filepath.Join(dir, "scenario.json")
	if fileExists(jsonPath) {
		c.ScenarioJSON = jsonPath
		// Đọc thông tin URL và Method mặc định từ JSON
		data, err := os.ReadFile(jsonPath)
		if err == nil {
			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err == nil {
				if u, ok := raw["url"].(string); ok && c.URL == "" {
					c.URL = u
				}
				if m, ok := raw["method"].(string); ok && c.Method == "GET" {
					c.Method = m
				}
			}
		}
	}

	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
