---
name: capsizer-remote
description: >-
  Điều phối và thực thi kiểm thử tải (Capacity Sizing & Open-Loop Load Test)
  từ một máy phát tải độc lập qua SSH, tự động thu thập log probe và ramp,
  sau đó đối chiếu và tạo báo cáo định chuẩn hiệu năng với report.go.
---

# Skill: Capsizer Remote Load Testing (Máy bắn tải từ xa qua SSH)

Skill này hướng dẫn và cung cấp công cụ tự động để Antigravity Agent điều phối quy trình kiểm thử tải phân tán:
1. Khởi động daemon `probe` lấy mẫu chỉ số cgroup/TCP trên Pod/Container mục tiêu.
2. Điều khiển máy phát tải từ xa (Dedicated Load Generator Host) qua SSH để bắn tải Open-Loop bằng `capsizer-ramp`.
3. Tải các file log `loadtest-ramp.jsonl` và `probe.jsonl` về máy trạm.
4. Chạy `report.go` để phân tích và lập bảng định chuẩn năng lực hệ thống.

---

## 1. Cấu hình kết nối (.env.remote)

Trước khi chạy, cần có file cấu hình `.env.remote` tại thư mục gốc của project (hoặc truyền qua biến môi trường):

```bash
# Thông tin máy phát tải từ xa
REMOTE_HOST="192.168.1.50"          # IP hoặc hostname máy bắn tải
REMOTE_USER="vbox"                  # SSH user
REMOTE_PORT="22"                    # Cổng SSH
REMOTE_KEY=""                       # Đường dẫn private key (nếu dùng key riêng, vd: ~/.ssh/id_rsa)
REMOTE_DIR="~/capsizer-runner"       # Thư mục làm việc trên máy bắn tải

# Thông số kiểm thử tải
TARGET_URL="http://10.0.0.10:8000/chat"
SCENARIO="chat-sse"                 # chat-sse, sample-get, sample-post, v.v.
STEPS="1,2,4,8,16"                  # Các mức RPS
STEP_SECONDS="60"                   # Thời gian mỗi bậc (giây)
COOLDOWN="20"                       # Thời gian nghỉ giữa 2 bậc (giây)
QUERIES_FILE=""                     # File câu hỏi (nếu có, vd: queries.txt)
USERS_CSV=""                        # File user pool (nếu có, vd: users.csv)

# Thông tin ứng dụng mục tiêu (Kubernetes Pod hoặc Docker/VM)
TARGET_TYPE="k8s"                   # "k8s" hoặc "ssh"
K8S_NAMESPACE="default"
K8S_POD="my-app-pod-xxxx"
TARGET_PORT="8000"                  # Cổng dịch vụ để probe đo Accept Queue
```

---

## 2. Quy trình thực hiện chuẩn 4 bước

Khi người dùng yêu cầu chạy test tải từ máy bắn tải từ xa, Agent thực hiện theo 4 bước sau:

### Bước 1: Kiểm tra kết nối SSH tới máy bắn tải
```bash
./.agents/skills/capsizer-remote/scripts/run_remote.sh --check
```

### Bước 2: Triển khai & Khởi động probe trên mục tiêu
- **Nếu là Kubernetes Pod:**
  ```bash
  # Biên dịch probe tĩnh (nếu chưa có) và đẩy vào pod
  ./.agents/skills/capsizer-remote/scripts/manage_probe.sh start
  ```
- Tiến trình `probe` sẽ chạy ngầm và ghi số liệu vào `/tmp/probe.jsonl`.

### Bước 3: Kích hoạt bắn tải trên máy từ xa
Chạy script điều phối:
```bash
./.agents/skills/capsizer-remote/scripts/run_remote.sh --run
```
Script sẽ:
- Biên dịch tĩnh `capsizer-ramp` (nếu cần) và rsync lên máy phát tải cùng các file queries/users.
- Khởi chạy quá trình phát tải Open-Loop trên máy từ xa qua SSH.
- Tự động kéo file `loadtest-ramp.jsonl` về thư mục kết quả cục bộ `results/<run_id>/`.

### Bước 4: Dừng probe, thu thập log & Tạo báo cáo sizing
1. Dừng probe và tải `probe.jsonl` về:
   ```bash
   ./.agents/skills/capsizer-remote/scripts/manage_probe.sh stop
   ```
2. Chạy `report.go` để phân tích:
   ```bash
   go run report.go \
     --ramp results/<run_id>/loadtest-ramp.jsonl \
     --probe results/<run_id>/probe.jsonl
   ```
3. Đọc kết quả phân tích: xác định $C$ (CPU cost/req), điểm gãy `knee point`, và khuyến nghị số lượng worker / CPU quota cho người dùng.

> [!TIP]
> Có thể chạy toàn bộ 4 bước tự động trong một lệnh duy nhất:
> ```bash
> ./.agents/skills/capsizer-remote/scripts/run_all.sh
> ```
