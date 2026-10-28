---
name: capsizer-two-phase-profiling
description: >-
  Tự động hóa quy trình kiểm thử tải và profiling 2 Pha (README Mục 7.4) theo kiến trúc 3 máy.
  Tách biệt cấu hình Hạ tầng (.env.remote) và Kịch bản khai báo (scenarios/<name>/scenario.json & scenario.env).
  Hỗ trợ chạy đơn lẻ hoặc chạy tự động hàng loạt nhiều scenario (Multi-Scenario Batch).
---

# Capsizer 2-Phase Capacity Sizing & Profiling Workflow (Kiến trúc 3 Máy & Declarative Scenarios)

Kỹ năng này tự động hóa toàn bộ quy trình kiểm thử tải và định chuẩn năng lực hệ thống theo chuẩn **2 Pha**, hỗ trợ triển khai phân tán **3 Máy độc lập** và kiến trúc **Kịch bản khai báo Declarative (tương tự k6)**:

```mermaid
flowchart TD
    subgraph Machine1["[MACHINE 1] MÁY CHẠY SKILL (Local Agent / Điều phối)"]
        Skill["Antigravity Skill\n(run_two_phase.sh / run_batch.sh)"]
        Report["capsizer-report\n(Tổng hợp Sizing & Đọc FlameGraph)"]
    end

    subgraph Configs["CẤU HÌNH TÁCH BIỆT"]
        InfraEnv[".env.remote\n(Hạ tầng kết nối 3 máy SSH)"]
        ScenariosDir["scenarios/<name>/\n├── scenario.json (Request, Headers, Template Body)\n└── scenario.env (Steps, K8s Selector, Profiler)"]
    end

    subgraph Machine2["[MACHINE 2] MÁY PHÁT TẢI (Dedicated Host qua SSH)"]
        Ramp["capsizer-ramp\n(Chạy Declarative Scenario độc lập)"]
        RampLog["phase1-ramp.jsonl"]
    end

    subgraph Machine3["[MACHINE 3] CỤM KUBERNETES TỪ XA"]
        K8sServer["Server Cụm K8s (Master / Bastion)\nSSH passwordless + sudo root"]
        subgraph TargetPod["TARGET POD"]
            App["App Server (FastAPI / Gin / ...)"]
            Sidecar["Debug Container / Sidecar\n(probe + py-spy / perf)"]
        end
        ProbeLog["probe.jsonl"]
        SVG["flamegraph.svg"]
    end

    InfraEnv -.-> Skill
    ScenariosDir -.-> Skill

    Skill ==>|"1. SSH scp binary & scenario.json"| Ramp
    Skill ==>|"2. SSH sudo kubectl chui vào Pod"| K8sServer
    K8sServer -.->|"gắn sidecar & đo"| Sidecar
    Ramp ==>|"3. Bắn tải Open-Loop HTTP / SSE"| App
    Sidecar -.->|"Lấy mẫu CPU, RAM, TCP"| App

    RampLog -->|"4. scp về"| Skill
    ProbeLog -->|"5. kubectl cp + scp về"| Skill
    SVG -->|"6. scp về (Pha 2)"| Skill
    Skill --> Report
```

---

## 1. Phân tách Cấu hình Chuẩn Kiến trúc

### A. Cấu hình Hạ tầng cố định (`.env.remote`)
Chỉ chứa thông tin kết nối giữa 3 máy, dùng chung cho mọi kịch bản:
```bash
# [Machine 2] Máy phát tải
LOADGEN_MODE="remote"
REMOTE_HOST="192.168.1.50"
REMOTE_USER="vbox"

# [Machine 3] Server Cụm K8s (SSH passwordless + sudo root để chạy kubectl)
K8S_SSH_HOST="192.168.1.100"
K8S_SSH_USER="vbox"
K8S_USE_SUDO="true"
K8S_NAMESPACE="default"
```

### B. Cấu hình Kịch bản độc lập (`scenarios/<tên_dịch_vụ>/`)
Mỗi kịch bản được tổ chức gọn gàng trong thư mục riêng:
- `scenario.json`: **[Khai báo Request tương tự k6]** Định nghĩa URL, HTTP method, headers, dynamic template body (`{{.Query}}`, `{{.Token}}`, `{{.UUID}}`), logic SSE.
- `scenario.env`: **[Thông số thực thi & K8s]** Dải bậc tải (`STEPS`), selector Pod mục tiêu (`K8S_SELECTOR`), loại profiler (`PROFILE_TYPE=pyspy|perf`).
- Tệp dữ liệu đi kèm (`queries.txt`, `users.csv`, `payload.json`) đặt ngay trong thư mục với đường dẫn tương đối.

*Ví dụ `scenarios/chatbot/`:*
- `scenario.json`:
  ```json
  {
    "name": "chat-sse",
    "type": "sse",
    "url": "http://10.0.0.10:8000/chat",
    "method": "POST",
    "headers": {
      "Content-Type": "application/json",
      "Authorization": "Bearer {{.Token}}"
    },
    "body": "{\"prompt\": \"{{.Query}}\", \"stream\": true}",
    "queries_file": "queries.txt",
    "users_file": "users.csv",
    "sse": { "end_marker": "[DONE]" }
  }
  ```
- `scenario.env`:
  ```bash
  K8S_SELECTOR="app=chatbot"
  PROFILE_TYPE="pyspy"               # Python FastAPI dùng py-spy
  STEPS="2,4,8,16,32"
  STEP_SECONDS="30"
  BASELINE_WAIT="60"
  COOLDOWN="15"
  ```

---

## 2. Quy trình Thực thi của Agent

### Trường hợp 1: Chạy một kịch bản đơn lẻ
Agent kích hoạt kịch bản theo tên thư mục:
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_two_phase.sh --scenario chatbot
```
*(Script sẽ tự động tìm nạp `scenarios/chatbot/scenario.env` + `scenario.json`, tự động đồng bộ file kịch bản và dữ liệu lên máy phát tải từ xa qua SSH).*

### Trường hợp 2: Kiểm tra kết nối trước khi chạy (Preflight Check)
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_two_phase.sh --scenario chatbot --check
```

### Trường hợp 3: Chạy TỰ ĐỘNG HÀNG LOẠT nhiều kịch bản (Batch Runner)
Khi người dùng yêu cầu: *"Hãy chạy kiểm thử tải lần lượt cả chatbot, sample-post và sample-get"*:
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_batch.sh chatbot sample-post sample-get
```
Hoặc quét và chạy toàn bộ kịch bản có trong thư mục `scenarios/`:
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_batch.sh
```
Sau khi hoàn thành tất cả các kịch bản, Agent sẽ đọc file tổng hợp `batch_summary.txt` để đưa ra bảng so sánh hiệu năng và năng lực tổng thể của tất cả các service cho người dùng.
