package scenarios

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ScenarioParams chứa toàn bộ các tham số cấu hình từ CLI / môi trường truyền vào
// để khởi tạo bất kỳ Scenario nào.
type ScenarioParams struct {
	TargetURL string
	Method    string
	Headers   map[string]string
	Timeout   time.Duration
	Options   map[string]string // Các tham số động truyền qua -opt key=value hoặc config
}

// GetOpt lấy giá trị option theo key, nếu không có thì trả về defaultVal.
func (p ScenarioParams) GetOpt(key, defaultVal string) string {
	if p.Options == nil {
		return defaultVal
	}
	if val, ok := p.Options[key]; ok && val != "" {
		return val
	}
	return defaultVal
}

// GetOptBool lấy giá trị boolean option (hỗ trợ true/1/yes).
func (p ScenarioParams) GetOptBool(key string, defaultVal bool) bool {
	val := p.GetOpt(key, "")
	if val == "" {
		return defaultVal
	}
	switch val {
	case "true", "1", "yes", "on", "TRUE", "True":
		return true
	case "false", "0", "no", "off", "FALSE", "False":
		return false
	}
	return defaultVal
}

// ScenarioFactory là hàm khởi tạo một Scenario từ ScenarioParams.
type ScenarioFactory func(params ScenarioParams) (Scenario, error)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]ScenarioFactory)
)

// Register đăng ký một kịch bản mới vào hệ thống (nếu dùng Go API).
func Register(name string, factory ScenarioFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Get khởi tạo một Scenario theo tên kịch bản hoặc file/thư mục scenario.json (tương tự k6).
func Get(name string, params ScenarioParams) (Scenario, error) {
	// 1. Kiểm tra trực tiếp file .json
	if strings.HasSuffix(name, ".json") {
		return LoadDeclarativeScenario(name, params)
	}

	// 2. Kiểm tra trực tiếp thư mục chứa scenario.json
	if fi, err := os.Stat(name); err == nil && fi.IsDir() {
		candidate := filepath.Join(name, "scenario.json")
		if _, err := os.Stat(candidate); err == nil {
			return LoadDeclarativeScenario(name, params)
		}
	}

	// 3. Tìm trong thư mục scenarios/<name> hoặc scenarios/<name>.json
	candidates := []string{
		filepath.Join("scenarios", name),
		filepath.Join("scenarios", name, "scenario.json"),
		filepath.Join("scenarios", name+".json"),
		name + ".json",
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil {
			if fi.IsDir() {
				if _, err := os.Stat(filepath.Join(c, "scenario.json")); err == nil {
					return LoadDeclarativeScenario(c, params)
				}
			} else {
				return LoadDeclarativeScenario(c, params)
			}
		}
	}

	// 4. Tìm kiếm trong các thư mục con của scenarios/ dựa trên trường "name" trong scenario.json
	if entries, err := os.ReadDir("scenarios"); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				cfgFile := filepath.Join("scenarios", entry.Name(), "scenario.json")
				if data, err := os.ReadFile(cfgFile); err == nil {
					var partial struct {
						Name string `json:"name"`
					}
					if err := json.Unmarshal(data, &partial); err == nil && partial.Name == name {
						return LoadDeclarativeScenario(filepath.Join("scenarios", entry.Name()), params)
					}
				}
			}
		}
	}

	// 5. Tra cứu trong registry Go (nếu có đăng ký bằng mã Go)
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if ok {
		return factory(params)
	}

	return nil, fmt.Errorf("không tìm thấy kịch bản '%s'. Các kịch bản khả dụng: %v (hoặc truyền trực tiếp file/thư mục scenario.json)", name, AvailableScenarios())
}

// AvailableScenarios trả về danh sách tên tất cả các kịch bản có sẵn.
func AvailableScenarios() []string {
	nameMap := make(map[string]bool)

	// Quét các kịch bản declarative trong scenarios/
	if entries, err := os.ReadDir("scenarios"); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				cfgFile := filepath.Join("scenarios", entry.Name(), "scenario.json")
				if data, err := os.ReadFile(cfgFile); err == nil {
					var partial struct {
						Name string `json:"name"`
					}
					if err := json.Unmarshal(data, &partial); err == nil && partial.Name != "" {
						nameMap[partial.Name] = true
					} else {
						nameMap[entry.Name()] = true
					}
				} else {
					nameMap[entry.Name()] = true
				}
			}
		}
	}

	// Thêm các kịch bản Go nếu có
	registryMu.RLock()
	for name := range registry {
		nameMap[name] = true
	}
	registryMu.RUnlock()

	names := make([]string, 0, len(nameMap))
	for name := range nameMap {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
