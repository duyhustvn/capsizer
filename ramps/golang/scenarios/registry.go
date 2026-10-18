package scenarios

import (
	"fmt"
	"sort"
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
	BodyData  []byte
	UserPool  *UserPool
	Queries   []string
	CacheBust bool
}

// ScenarioFactory là hàm khởi tạo một Scenario từ ScenarioParams.
type ScenarioFactory func(params ScenarioParams) (Scenario, error)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]ScenarioFactory)
)

// Register đăng ký một kịch bản mới vào hệ thống.
// Bất kỳ kịch bản nào mới (file .go mới) chỉ cần gọi Register(...) trong hàm init()
// là có thể tự động nhận diện bởi main runner và Docker base image mà không cần sửa main.go.
func Register(name string, factory ScenarioFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Get khởi tạo một Scenario theo tên kịch bản đã đăng ký.
func Get(name string, params ScenarioParams) (Scenario, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("không tìm thấy kịch bản '%s'. Các kịch bản khả dụng: %v", name, AvailableScenarios())
	}
	return factory(params)
}

// AvailableScenarios trả về danh sách tên tất cả các kịch bản đã đăng ký vào hệ thống.
func AvailableScenarios() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
