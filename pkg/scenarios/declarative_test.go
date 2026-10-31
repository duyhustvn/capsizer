package scenarios

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenericDatasets(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "capsizer-scenario-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 1. Tạo file CSV: users.csv
	usersCSV := "user_id,token,tier\nu101,tok-alice,gold\nu102,tok-bob,silver\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "users.csv"), []byte(usersCSV), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Tạo file lines: prompts.txt
	promptsTXT := "Xin chào AI\nThủ đô Việt Nam là gì?\nCapsizer stress testing\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "prompts.txt"), []byte(promptsTXT), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Tạo file JSONL: items.jsonl
	itemsJSONL := `{"sku": "SKU-A", "price": 10.5}` + "\n" + `{"sku": "SKU-B", "price": 20.0}` + "\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "items.jsonl"), []byte(itemsJSONL), 0644); err != nil {
		t.Fatal(err)
	}

	// 4. Tạo file JSON: config.json
	configJSON := `[{"cluster": "asia-east1"}, {"cluster": "us-central1"}]`
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// 5. Tạo mock HTTP Server để nhận request
	receivedRequests := make([]string, 0)
	receivedTokens := make([]string, 0)
	receivedBodies := make([]string, 0)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRequests = append(receivedRequests, r.URL.Path)
		receivedTokens = append(receivedTokens, r.Header.Get("Authorization"))

		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBodies = append(receivedBodies, string(buf[:n]))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer ts.Close()

	// 6. Tạo scenario.json sử dụng Generic Datasets
	scenarioJSON := fmt.Sprintf(`{
		"name": "generic-test",
		"type": "http",
		"url": "%s/test/{{.items.sku}}",
		"method": "POST",
		"headers": {
			"Authorization": "Bearer {{.users.token}}",
			"X-User-ID": "{{.users.user_id}}",
			"X-Tier": "{{.users.tier}}"
		},
		"body": "{\"prompt\": {{quote .prompts}}, \"cluster\": \"{{.clusters.cluster}}\"}",
		"datasets": {
			"users": {
				"file": "users.csv",
				"mode": "sequential"
			},
			"prompts": {
				"file": "prompts.txt",
				"mode": "random"
			},
			"items": {
				"file": "items.jsonl",
				"mode": "sequential"
			},
			"clusters": {
				"file": "config.json",
				"mode": "sequential"
			}
		},
		"expected_status": [200]
	}`, ts.URL)

	scenarioPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scenarioPath, []byte(scenarioJSON), 0644); err != nil {
		t.Fatal(err)
	}

	params := ScenarioParams{
		TargetURL: "",
		Options:   map[string]string{},
	}

	scenario, err := LoadDeclarativeScenario(scenarioPath, params)
	if err != nil {
		t.Fatalf("LoadDeclarativeScenario failed: %v", err)
	}

	if err := scenario.Setup(context.Background(), ts.Client()); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	// Thực hiện 2 requests kiểm tra tuần tự (sequential) và ngẫu nhiên (random)
	rec1 := scenario.Execute(context.Background(), ts.Client())
	if !rec1.Ok {
		t.Fatalf("Request 1 failed: %v", *rec1.Err)
	}

	rec2 := scenario.Execute(context.Background(), ts.Client())
	if !rec2.Ok {
		t.Fatalf("Request 2 failed: %v", *rec2.Err)
	}

	// Kiểm tra Request 1: sequential users -> u101 / tok-alice
	if len(receivedTokens) < 2 {
		t.Fatalf("Expected 2 requests, got %d", len(receivedTokens))
	}
	if receivedTokens[0] != "Bearer tok-alice" {
		t.Errorf("Request 1 token mismatch: expected 'Bearer tok-alice', got '%s'", receivedTokens[0])
	}
	// Request 2: sequential users -> u102 / tok-bob
	if receivedTokens[1] != "Bearer tok-bob" {
		t.Errorf("Request 2 token mismatch: expected 'Bearer tok-bob', got '%s'", receivedTokens[1])
	}

	// Kiểm tra sequential items: SKU-A rồi đến SKU-B
	if receivedRequests[0] != "/test/SKU-A" {
		t.Errorf("Request 1 URL path mismatch: expected '/test/SKU-A', got '%s'", receivedRequests[0])
	}
	if receivedRequests[1] != "/test/SKU-B" {
		t.Errorf("Request 2 URL path mismatch: expected '/test/SKU-B', got '%s'", receivedRequests[1])
	}

	// Kiểm tra body có quote chuẩn xác
	if !strings.Contains(receivedBodies[0], `"prompt": "`) {
		t.Errorf("Body should contain quoted prompt: %s", receivedBodies[0])
	}
	if !strings.Contains(receivedBodies[0], `"cluster": "asia-east1"`) {
		t.Errorf("Body should contain cluster asia-east1: %s", receivedBodies[0])
	}
	if !strings.Contains(receivedBodies[1], `"cluster": "us-central1"`) {
		t.Errorf("Body should contain cluster us-central1: %s", receivedBodies[1])
	}
}

func TestCacheBusting(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "_cb=") {
			t.Errorf("Expected query parameter _cb in URL, got: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tmpDir, err := os.MkdirTemp("", "capsizer-cachebust-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	scenarioJSON := fmt.Sprintf(`{
		"name": "cachebust-test",
		"type": "http",
		"url": "%s/api/data?filter=active",
		"cache_bust": true
	}`, ts.URL)
	scPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scPath, []byte(scenarioJSON), 0644); err != nil {
		t.Fatal(err)
	}

	sc, err := LoadDeclarativeScenario(scPath, ScenarioParams{})
	if err != nil {
		t.Fatalf("LoadDeclarativeScenario failed: %v", err)
	}

	rec := sc.Execute(context.Background(), ts.Client())
	if !rec.Ok {
		t.Fatalf("Request failed: %v", *rec.Err)
	}
}

func TestCLIOverrides(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "capsizer-override-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	origPrompts := "Prompt Gốc\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "orig_prompts.txt"), []byte(origPrompts), 0644); err != nil {
		t.Fatal(err)
	}

	customPrompts := "Prompt Ghi Đè Qua CLI\n"
	customPath := filepath.Join(tmpDir, "custom_prompts.txt")
	if err := os.WriteFile(customPath, []byte(customPrompts), 0644); err != nil {
		t.Fatal(err)
	}

	scenarioJSON := `{
		"name": "override-test",
		"type": "http",
		"url": "http://127.0.0.1/test",
		"datasets": {
			"prompts": {
				"file": "orig_prompts.txt",
				"mode": "sequential"
			}
		}
	}`
	scPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scPath, []byte(scenarioJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Ghi đè file dataset qua Options: -opt dataset.prompts.file=<customPath>
	params := ScenarioParams{
		Options: map[string]string{
			"dataset.prompts.file": customPath,
		},
	}
	sc, err := LoadDeclarativeScenario(scPath, params)
	if err != nil {
		t.Fatalf("LoadDeclarativeScenario failed: %v", err)
	}

	ds, ok := sc.datasets["prompts"]
	if !ok {
		t.Fatal("dataset 'prompts' missing")
	}
	if ds.FilePath != customPath {
		t.Errorf("Expected prompts file to be overridden to %s, got %s", customPath, ds.FilePath)
	}
	record := ds.Next()
	if fmt.Sprint(record) != "Prompt Ghi Đè Qua CLI" {
		t.Errorf("Expected prompt content to be overridden, got: %v", record)
	}
}

func TestSSEExecution(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: Xin chao\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	tmpDir, err := os.MkdirTemp("", "capsizer-sse-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	scenarioJSON := fmt.Sprintf(`{
		"name": "sse-test",
		"type": "sse",
		"url": "%s/stream",
		"method": "POST",
		"sse": {
			"end_marker": "[DONE]"
		}
	}`, ts.URL)
	scPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scPath, []byte(scenarioJSON), 0644); err != nil {
		t.Fatal(err)
	}

	sc, err := LoadDeclarativeScenario(scPath, ScenarioParams{})
	if err != nil {
		t.Fatalf("LoadDeclarativeScenario failed: %v", err)
	}

	rec := sc.Execute(context.Background(), ts.Client())
	if !rec.Ok {
		t.Fatalf("SSE request failed: %v", *rec.Err)
	}
	if rec.EndSeen == nil || !*rec.EndSeen {
		t.Errorf("Expected EndSeen to be true for SSE stream")
	}
	if rec.TtftMs == nil || *rec.TtftMs <= 0 {
		t.Errorf("Expected TTFT to be recorded")
	}
}

func TestMissingFileError(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "capsizer-missing-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	scenarioJSON := `{
		"name": "missing-file-test",
		"url": "http://127.0.0.1/test",
		"datasets": {
			"nonexistent": {
				"file": "does_not_exist.csv"
			}
		}
	}`
	scPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scPath, []byte(scenarioJSON), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = LoadDeclarativeScenario(scPath, ScenarioParams{})
	if err == nil {
		t.Fatal("Expected error for missing dataset file, got nil")
	}
	if !strings.Contains(err.Error(), "does_not_exist.csv") {
		t.Errorf("Error should mention missing file name, got: %v", err)
	}
}
