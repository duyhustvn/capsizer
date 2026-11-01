package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config chứa toàn bộ thông số hạ tầng 3 máy và kịch bản thực thi.
type Config struct {
	// [Machine 1] Skill Local
	ProjectRoot string
	EnvFile     string
	OutDir      string
	OnlyCheck   bool

	// [Machine 2] Máy phát tải (Load Generator)
	LoadgenMode string // "local" hoặc "remote"
	LoadgenHost string
	LoadgenUser string
	LoadgenPort int
	LoadgenKey  string
	LoadgenJump string
	LoadgenDir  string

	// [Machine 3] Server Cụm K8s
	K8sSSHHost       string
	K8sSSHUser       string
	K8sSSHPort       int
	K8sSSHKey        string
	K8sSSHJump       string
	K8sUseSudo       bool
	K8sNamespace     string
	K8sPod           string
	K8sSelector      string
	K8sContainer     string
	K8sMode          string // "debug", "sidecar", "cp"
	SidecarImage     string
	SidecarContainer string
	SetPodCPU        string // ví dụ "1" hoặc "1000m"
	RestorePodCPU    bool   // mặc định true
	TargetPort       int
	PortExplicit     bool
	TargetPID        int
	KubeconfigPath   string
	ContextName      string

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
// Lưu ý: LoadgenUser/Port và K8sSSHUser/Port để trống ("" / 0) mặc định để tôn trọng ~/.ssh/config
// (bao gồm User, Port, IdentityFile, ProxyJump khai báo trong ~/.ssh/config).
func NewDefaultConfig() *Config {
	return &Config{
		ProjectRoot:      FindProjectRoot(),
		LoadgenMode:      "local",
		LoadgenDir:       "~/capsizer-loadtest",
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

// FindProjectRoot tự động tìm thư mục gốc của dự án capsizer.
func FindProjectRoot() string {
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			exe = realExe
		}
		candidates = append(candidates, filepath.Dir(exe))
	}

	for _, start := range candidates {
		dir := start
		for i := 0; i < 6; i++ {
			if fileExists(filepath.Join(dir, "go.mod")) ||
				fileExists(filepath.Join(dir, ".env.remote")) ||
				fileExists(filepath.Join(dir, ".env.remote.example")) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// LoadEnvFile đọc một file .env và trả về map key-value, tự động loại bỏ inline comment (# ...) và dấu nháy.
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
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			if k == "" {
				continue
			}
			v := parseEnvValue(parts[1])
			res[k] = v
		}
	}
	return res, scanner.Err()
}

// parseEnvValue xử lý chuỗi giá trị sau dấu '=', bóc tách phần trong nháy và loại bỏ comment '# ...' cuối dòng.
func parseEnvValue(raw string) string {
	val := strings.TrimSpace(raw)
	if val == "" {
		return ""
	}

	// Nếu bắt đầu bằng dấu nháy kép hoặc nháy đơn
	if val[0] == '"' || val[0] == '\'' {
		quote := val[0]
		endIdx := findClosingQuote(val, quote)
		if endIdx != -1 {
			inner := val[1:endIdx]
			if quote == '"' {
				inner = strings.ReplaceAll(inner, `\"`, `"`)
				inner = strings.ReplaceAll(inner, `\\`, `\`)
			}
			return inner
		}
		// Fallback nếu người dùng quên đóng nháy
		return strings.Trim(val, `"'`)
	}

	// Không có dấu nháy bao ngoài: cắt bỏ inline comment nếu '#' đứng đầu hoặc đứng sau khoảng trắng
	for i := 0; i < len(val); i++ {
		if val[i] == '#' && (i == 0 || val[i-1] == ' ' || val[i-1] == '\t') {
			val = val[:i]
			break
		}
	}
	return strings.TrimSpace(val)
}

func findClosingQuote(s string, quote byte) int {
	escaped := false
	for i := 1; i < len(s); i++ {
		if quote == '"' && s[i] == '\\' && !escaped {
			escaped = true
			continue
		}
		if s[i] == quote && !escaped {
			return i
		}
		escaped = false
	}
	return -1
}

// ResolveAndApplyInfraEnv tự động tìm và nạp file .env.remote (hoặc file chỉ định qua --env-file).
func (c *Config) ResolveAndApplyInfraEnv(explicitEnvFile string) error {
	// 1. Nạp biến môi trường từ OS shell trước (nếu có export)
	c.ApplyOSEnv()

	if explicitEnvFile == "" {
		explicitEnvFile = os.Getenv("CAPSIZER_ENV_FILE")
	}

	if explicitEnvFile != "" {
		if !fileExists(explicitEnvFile) {
			return fmt.Errorf("không tìm thấy file cấu hình hạ tầng: %s", explicitEnvFile)
		}
		c.EnvFile = explicitEnvFile
		c.ApplyInfraEnv(explicitEnvFile)
		return nil
	}

	// 2. Tự động dò tìm .env.remote và .env ở thư mục hiện tại hoặc ProjectRoot
	searchPaths := []string{
		".env.remote",
	}
	if c.ProjectRoot != "" && c.ProjectRoot != "." {
		searchPaths = append(searchPaths, filepath.Join(c.ProjectRoot, ".env.remote"))
	}
	searchPaths = append(searchPaths, ".env")
	if c.ProjectRoot != "" && c.ProjectRoot != "." {
		searchPaths = append(searchPaths, filepath.Join(c.ProjectRoot, ".env"))
	}

	for _, p := range searchPaths {
		if fileExists(p) {
			c.EnvFile = p
			c.ApplyInfraEnv(p)
			break
		}
	}
	return nil
}

// ApplyOSEnv nạp các biến môi trườngCapsizer được export trực tiếp trên shell.
func (c *Config) ApplyOSEnv() {
	keys := []string{
		"LOADGEN_MODE", "REMOTE_HOST", "LOADGEN_HOST", "REMOTE_USER", "LOADGEN_USER",
		"REMOTE_PORT", "LOADGEN_PORT", "REMOTE_KEY", "LOADGEN_KEY", "REMOTE_JUMP", "LOADGEN_JUMP",
		"REMOTE_DIR", "LOADGEN_DIR",
		"K8S_SSH_HOST", "K8S_SSH_USER", "K8S_SSH_PORT", "K8S_SSH_KEY", "K8S_SSH_JUMP", "K8S_USE_SUDO",
		"K8S_NAMESPACE", "K8S_POD", "K8S_SELECTOR", "K8S_CONTAINER", "K8S_MODE",
		"SIDECAR_IMAGE", "SIDECAR_CONTAINER", "K8S_KUBECONFIG", "KUBECONFIG", "K8S_CONTEXT",
		"SET_POD_CPU", "RESTORE_POD_CPU", "PROFILE_TYPE", "TARGET_PORT", "TARGET_PID",
		"STEPS", "STEP_SECONDS", "BASELINE_WAIT", "COOLDOWN", "SETTLE", "TIMEOUT",
		"MAX_INFLIGHT", "CACHE_BUST", "SKIP_VERIFY", "TARGET_URL",
	}
	envMap := make(map[string]string)
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			envMap[k] = parseEnvValue(v)
		}
	}
	if len(envMap) > 0 {
		c.applyEnvMap(envMap, ".")
	}
}

// ApplyInfraEnv áp dụng cấu hình từ .env.remote.
func (c *Config) ApplyInfraEnv(path string) {
	envMap, err := LoadEnvFile(path)
	if err != nil {
		return
	}
	c.applyEnvMap(envMap, filepath.Dir(path))
}

// ApplyScenarioEnv áp dụng cấu hình từ scenario.env.
func (c *Config) ApplyScenarioEnv(path string) {
	envMap, err := LoadEnvFile(path)
	if err != nil {
		return
	}
	c.applyEnvMap(envMap, filepath.Dir(path))
}

func (c *Config) applyEnvMap(envMap map[string]string, baseDir string) {
	// [Machine 2] Load Generator
	if v, ok := envMap["LOADGEN_MODE"]; ok && v != "" {
		c.LoadgenMode = v
	}
	if v, ok := envMap["REMOTE_HOST"]; ok && v != "" {
		c.LoadgenHost = v
	}
	if v, ok := envMap["LOADGEN_HOST"]; ok && v != "" {
		c.LoadgenHost = v
	}
	if v, ok := envMap["REMOTE_USER"]; ok && v != "" {
		c.LoadgenUser = v
	}
	if v, ok := envMap["LOADGEN_USER"]; ok && v != "" {
		c.LoadgenUser = v
	}
	if v, ok := envMap["REMOTE_PORT"]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			c.LoadgenPort = p
		}
	}
	if v, ok := envMap["LOADGEN_PORT"]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			c.LoadgenPort = p
		}
	}
	if v, ok := envMap["REMOTE_KEY"]; ok {
		c.LoadgenKey = v
	}
	if v, ok := envMap["LOADGEN_KEY"]; ok && v != "" {
		c.LoadgenKey = v
	}
	if v, ok := envMap["REMOTE_JUMP"]; ok {
		c.LoadgenJump = v
	}
	if v, ok := envMap["LOADGEN_JUMP"]; ok && v != "" {
		c.LoadgenJump = v
	}
	if v, ok := envMap["REMOTE_DIR"]; ok && v != "" {
		c.LoadgenDir = v
	}
	if v, ok := envMap["LOADGEN_DIR"]; ok && v != "" {
		c.LoadgenDir = v
	}

	// [Machine 3] K8s Target
	if v, ok := envMap["K8S_SSH_HOST"]; ok && v != "" {
		c.K8sSSHHost = v
	}
	if v, ok := envMap["K8S_SSH_USER"]; ok {
		c.K8sSSHUser = v
	}
	if v, ok := envMap["K8S_SSH_PORT"]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			c.K8sSSHPort = p
		}
	}
	if v, ok := envMap["K8S_SSH_KEY"]; ok {
		c.K8sSSHKey = v
	}
	if v, ok := envMap["K8S_SSH_JUMP"]; ok {
		c.K8sSSHJump = v
	}
	if v, ok := envMap["K8S_USE_SUDO"]; ok && v != "" {
		c.K8sUseSudo = parseBool(v, true)
	}
	if v, ok := envMap["K8S_NAMESPACE"]; ok && v != "" {
		c.K8sNamespace = v
	}
	if v, ok := envMap["K8S_POD"]; ok && v != "" {
		c.K8sPod = v
	}
	if v, ok := envMap["K8S_SELECTOR"]; ok && v != "" {
		c.K8sSelector = v
	}
	if v, ok := envMap["K8S_CONTAINER"]; ok && v != "" {
		c.K8sContainer = v
	}
	if v, ok := envMap["K8S_MODE"]; ok && v != "" {
		c.K8sMode = v
	}
	if v, ok := envMap["SIDECAR_IMAGE"]; ok && v != "" {
		c.SidecarImage = v
	}
	if v, ok := envMap["SIDECAR_CONTAINER"]; ok && v != "" {
		c.SidecarContainer = v
	}
	if v, ok := envMap["KUBECONFIG"]; ok && v != "" {
		c.KubeconfigPath = v
	}
	if v, ok := envMap["K8S_KUBECONFIG"]; ok && v != "" {
		c.KubeconfigPath = v
	}
	if v, ok := envMap["K8S_CONTEXT"]; ok && v != "" {
		c.ContextName = v
	}

	// [Scenario & Sizing]
	if v, ok := envMap["SET_POD_CPU"]; ok && v != "" {
		c.SetPodCPU = v
	}
	if v, ok := envMap["RESTORE_POD_CPU"]; ok && v != "" {
		c.RestorePodCPU = parseBool(v, true)
	}
	if v, ok := envMap["PROFILE_TYPE"]; ok && v != "" {
		c.ProfileType = v
	}
	if v, ok := envMap["TARGET_PORT"]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			c.TargetPort = p
			c.PortExplicit = true
		}
	}
	if v, ok := envMap["TARGET_PID"]; ok && v != "" {
		if pid, err := strconv.Atoi(v); err == nil && pid >= 0 {
			c.TargetPID = pid
		}
	}
	if v, ok := envMap["STEPS"]; ok && v != "" {
		c.Steps = v
	}
	if v, ok := envMap["STEP_SECONDS"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.StepSeconds = n
		}
	}
	if v, ok := envMap["BASELINE_WAIT"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.BaselineWait = n
		}
	}
	if v, ok := envMap["SETTLE"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Cooldown = n
		}
	}
	if v, ok := envMap["COOLDOWN"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Cooldown = n
		}
	}
	if v, ok := envMap["TIMEOUT"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Timeout = n
		}
	}
	if v, ok := envMap["MAX_INFLIGHT"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxInflight = n
		}
	}
	if v, ok := envMap["CACHE_BUST"]; ok && v != "" {
		c.CacheBust = parseBool(v, false)
	}
	if v, ok := envMap["SKIP_VERIFY"]; ok && v != "" {
		c.SkipVerify = parseBool(v, false)
	}
	if v, ok := envMap["TARGET_URL"]; ok && v != "" {
		c.URL = v
	}
	if v, ok := envMap["URL"]; ok && v != "" {
		c.URL = v
	}
	if v, ok := envMap["METHOD"]; ok && v != "" {
		c.Method = v
	}
	if v, ok := envMap["BODY"]; ok && v != "" {
		c.Body = v
	}
	if v, ok := envMap["BODY_FILE"]; ok && v != "" {
		if !filepath.IsAbs(v) && baseDir != "" {
			v = filepath.Join(baseDir, v)
		}
		c.BodyFile = v
	}
}

func parseBool(v string, defaultVal bool) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "true", "1", "yes", "y":
		return true
	case "false", "0", "no", "n":
		return false
	default:
		return defaultVal
	}
}

// LoadScenarioPackage tự động tìm và nạp kịch bản theo tên thư mục.
func (c *Config) LoadScenarioPackage(name string) error {
	if name == "" {
		return nil
	}

	candidates := []string{
		filepath.Join("scenarios", name),
	}
	if c.ProjectRoot != "" && c.ProjectRoot != "." {
		candidates = append(candidates, filepath.Join(c.ProjectRoot, "scenarios", name))
	}
	candidates = append(candidates, name)

	var dir string
	for _, cand := range candidates {
		if dirExists(cand) {
			dir = cand
			break
		}
	}
	if dir == "" {
		return fmt.Errorf("không tìm thấy thư mục kịch bản: %s", name)
	}

	c.Scenario = filepath.Base(dir)
	c.ScenarioDir = dir

	// 1. Nạp scenario.json trước để lấy URL và Method mặc định của kịch bản
	jsonPath := filepath.Join(dir, "scenario.json")
	if fileExists(jsonPath) {
		c.ScenarioJSON = jsonPath
		data, err := os.ReadFile(jsonPath)
		if err == nil {
			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err == nil {
				if u, ok := raw["url"].(string); ok && u != "" {
					c.URL = u
				}
				if m, ok := raw["method"].(string); ok && m != "" {
					c.Method = m
				}
			}
		}
	}

	// 2. Nạp scenario.env (có thể ghi đè URL/Method từ scenario.json nếu cần)
	envPath := filepath.Join(dir, "scenario.env")
	if !fileExists(envPath) {
		envPath = filepath.Join(dir, ".env")
	}
	if fileExists(envPath) {
		c.ScenarioEnv = envPath
		c.ApplyScenarioEnv(envPath)
	}

	return nil
}

// Finalize hoàn tất cấu hình sau khi đã nạp env files, scenario và CLI flags.
func (c *Config) Finalize() {
	if !c.PortExplicit && c.URL != "" {
		if p := extractPortFromURL(c.URL); p > 0 {
			c.TargetPort = p
		}
	}
	if c.TargetPort <= 0 {
		c.TargetPort = 8000
	}
}

func extractPortFromURL(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err == nil && u.Host != "" {
		if portStr := u.Port(); portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
				return p
			}
		}
		if strings.EqualFold(u.Scheme, "https") {
			return 443
		}
		if strings.EqualFold(u.Scheme, "http") {
			return 80
		}
	}
	return 8000
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
