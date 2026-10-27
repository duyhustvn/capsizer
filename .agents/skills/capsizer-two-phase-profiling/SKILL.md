---
name: capsizer-two-phase-profiling
description: >-
  Tự động hóa quy trình kiểm thử tải và profiling 2 Pha (README Mục 7.4):
  Pha 1 đo Capacity Sizing sạch (CPU, RAM, điểm gãy Knee Point);
  Pha 2 tự động phát tải tại điểm gãy và trích xuất FlameGraph SVG để điều tra sâu.
  Hỗ trợ linh hoạt máy phát tải Cục bộ (Local) hoặc Từ xa qua SSH (Remote Dedicated Load Generator).
---

# Capsizer 2-Phase Capacity Sizing & Profiling Workflow

Kỹ năng này tự động hóa toàn bộ quy trình kiểm thử tải và định chuẩn năng lực hệ thống theo chuẩn **2 Pha**:

## 1. Nguyên tắc cốt lõi (BẮT BUỘC TUÂN THỦ)
1. **Pha 1 (Capacity Sizing sạch):** Tuyệt đối **KHÔNG** bật `perf` hay `py-spy` trong khi đo sizing để tránh làm sai lệch chỉ số $C$ (ms CPU/req) và tránh gây ra điểm gãy ảo (False Knee Point do profiler chiếm CPU cgroup).
2. **Pha 2 (Root-Cause Profiling):** Chỉ kích hoạt FlameGraph khi Pha 1 đã xác định được điểm gãy (Knee Point), bằng cách giữ tải đều tại đúng bậc gãy trong 30–45s và lấy mẫu stack trace.

---

## 2. Mô hình máy phát tải (Load Generator)
Hỗ trợ 2 chế độ điều phối qua cờ `--loadgen-mode`:

1. **`--loadgen-mode local` (Mặc định):**
   - Chạy trực tiếp bộ phát tải `cmd/ramp` trên máy hiện tại.
2. **`--loadgen-mode remote`:**
   - Dành cho máy phát tải độc lập (Dedicated Load Generator Host qua SSH).
   - Tự động biên dịch tĩnh binary `cmd/ramp` (Linux amd64), đẩy lên máy từ xa qua SSH, kích hoạt phát tải và kéo file log kết quả về máy phân tích.
   - Cấu hình qua cờ hoặc khai báo trong `.env.remote`:
     `REMOTE_HOST`, `REMOTE_USER`, `REMOTE_PORT`, `REMOTE_KEY`, `REMOTE_DIR`.

---

## 3. Cơ chế hoạt động trên Kubernetes (K8s Pod Mục Tiêu):
Thông thường container của ứng dụng (Python slim, Golang distroless, Alpine) **không có sẵn công cụ profiling** (`py-spy`, `perf`, `flamegraph.pl`). Skill này hỗ trợ 3 cơ chế linh hoạt qua cờ `--k8s-mode`:

1. **`--k8s-mode debug` (Mặc định & Khuyên dùng):**
   - Tự động dùng `kubectl debug` gắn một **Ephemeral Container** sử dụng image `docker.io/duyle95/profiling-sidecar:latest` vào Pod mục tiêu.
   - Container debug này được cấp quyền `SYS_PTRACE`, chia sẻ Process Namespace với container ứng dụng (`--target`).
   - Có sẵn toàn bộ `probe`, `py-spy`, `perf`, và `FlameGraph` mà không cần cài đặt gì thêm vào ứng dụng.
2. **`--k8s-mode sidecar`:**
   - Dùng khi Pod Deployment đã khai báo sẵn một container sidecar (image `profiling-sidecar`).
3. **`--k8s-mode cp`:**
   - Biên dịch tĩnh binary `cmd/probe` và copy trực tiếp vào `/tmp/` của container ứng dụng qua `kubectl cp`.

---

## 4. Các bước thực hiện của Agent:

### Bước 1: Xác định tham số và chế độ chạy
- **`--url`**: URL dịch vụ nhận tải.
- **`--scenario`**: `chat-sse`, `sample-get`, `sample-post` hoặc kịch bản dịch vụ tùy chỉnh.
- **`--steps`**: Dải bậc RPS (mặc định: `2,4,8,16,32`).
- **`--profile-type`**: `pyspy` (nếu app là Python/FastAPI) hoặc `perf` (Go, C++, Java, Rust).
- **`--loadgen-mode`**: `local` hoặc `remote`.
- **`--pod` & `--namespace`**: Tên Pod và Namespace mục tiêu trên K8s (nếu target là K8s).

### Bước 2: Thực thi Script điều phối 2 Pha

#### Trường hợp A: Phát tải từ MÁY TỪ XA (Remote Load Generator qua SSH)
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_two_phase.sh \
  --loadgen-mode "remote" \
  --loadgen-host "192.168.1.50" \
  --url "http://10.0.0.10:8000/chat" \
  --scenario "chat-sse" \
  --steps "2,4,8,16,32" \
  --profile-type "pyspy" \
  --pod "<POD_NAME>" \
  --namespace "<NAMESPACE>" \
  --container "<APP_CONTAINER_NAME>" \
  --k8s-mode "debug"
```
*(Nếu đã cấu hình file `.env.remote`, chỉ cần thêm `--loadgen-mode remote` hoặc cấu hình `LOADGEN_MODE="remote"` trong file).*

#### Trường hợp B: Phát tải CỤC BỘ (Local)
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_two_phase.sh \
  --loadgen-mode "local" \
  --url "http://127.0.0.1:8000/chat" \
  --scenario "chat-sse" \
  --steps "2,4,8,16,32" \
  --profile-type "pyspy" \
  --pod "<POD_NAME>" \
  --namespace "<NAMESPACE>" \
  --container "<APP_CONTAINER_NAME>" \
  --k8s-mode "debug"
```

### Bước 3: Phân tích kết quả và phản hồi cho User
Sau khi script hoàn tất, đọc file báo cáo `phase1-report.txt` và trả về kết quả:

1. **Định chuẩn CPU & Năng lực:**
   - Chỉ số **$C$ (ms CPU / request)**: Chi phí CPU thuần cho mỗi request.
   - **Trần lý thuyết 1 worker (1 core)**: Tối đa bao nhiêu req/s.
   - **Trần lý thuyết Pod**: Dựa trên CPU quota được cấp.
   - **Số worker khuyến nghị**: Cần bao nhiêu worker để khai thác hết quota.
2. **Định chuẩn Bộ nhớ (RAM):**
   - **RAM nền (Baseline)**: Mức RAM khi rỗi.
   - **RAM đỉnh (Peak RAM)**: Mức RAM cao nhất quan sát được khi có tải.
   - **Khuyến nghị Kubernetes Pod**: Mức `requests.memory` và `limits.memory` (kèm 30% buffer an toàn).
   - **Cảnh báo rò rỉ RAM**: Có hiện tượng RAM tăng liên tục qua các bậc không?
3. **Chẩn đoán Điểm Gãy & FlameGraph (nếu có Pha 2):**
   - Điểm gãy quan sát được tại mức bao nhiêu req/s và nguyên nhân (Achieved throughput, TCP Accept Queue nghẽn, CFS Throttling, hay Health latency).
   - Dẫn link file FlameGraph `.svg` (ví dụ `flamegraph_knee_16rps.svg`) và nêu tên các hàm chiếm CPU nhiều nhất.
