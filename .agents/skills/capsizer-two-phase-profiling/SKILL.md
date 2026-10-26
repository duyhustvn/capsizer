---
name: capsizer-two-phase-profiling
description: >-
  Tự động hóa quy trình kiểm thử tải và profiling 2 Pha (README Mục 7.4):
  Pha 1 đo Capacity Sizing sạch (CPU, RAM, điểm gãy Knee Point);
  Pha 2 tự động phát tải tại điểm gãy và trích xuất FlameGraph SVG để điều tra sâu.
  Kích hoạt khi người dùng muốn đo hiệu năng, capacity sizing hoặc profile tối ưu hóa Pod/Service.
---

# Capsizer 2-Phase Capacity Sizing & Profiling Workflow

Kỹ năng này tự động hóa toàn bộ quy trình kiểm thử tải và định chuẩn năng lực hệ thống theo chuẩn **2 Pha**:

## Nguyên tắc cốt lõi (BẮT BUỘC TUÂN THỦ)
1. **Pha 1 (Capacity Sizing sạch):** Tuyệt đối **KHÔNG** bật `perf` hay `py-spy` trong khi đo sizing để tránh làm sai lệch chỉ số $C$ (ms CPU/req) và tránh gây ra điểm gãy ảo (False Knee Point do profiler chiếm CPU cgroup).
2. **Pha 2 (Root-Cause Profiling):** Chỉ kích hoạt FlameGraph khi Pha 1 đã xác định được điểm gãy (Knee Point), bằng cách giữ tải đều tại đúng bậc gãy trong 30–45s và lấy mẫu stack trace.

---

## Cơ chế hoạt động trên Kubernetes (K8s Pod):

Thông thường container của ứng dụng (Python slim, Golang distroless, Alpine) **không có sẵn công cụ profiling** (`py-spy`, `perf`, `flamegraph.pl`). Skill này hỗ trợ 3 cơ chế linh hoạt qua cờ `--k8s-mode`:

1. **`--k8s-mode debug` (Mặc định & Khuyên dùng):**
   - Tự động dùng `kubectl debug` gắn một **Ephemeral Container** sử dụng image `docker.io/duyle95/profiling-sidecar:latest` vào Pod mục tiêu.
   - Container debug này được cấp quyền `SYS_PTRACE`, chia sẻ Process Namespace với container ứng dụng (`--target`).
   - Có sẵn toàn bộ `probe`, `py-spy`, `perf`, và `FlameGraph` mà không cần cài đặt gì thêm vào ứng dụng.
   - Pha 1 chạy `probe` trong debug container, Pha 2 chạy `py-spy`/`perf` và kéo file `.svg` về máy host.

2. **`--k8s-mode sidecar`:**
   - Dùng khi Pod Deployment đã khai báo sẵn một container sidecar (ví dụ container tên `sidecar` dùng image `profiling-sidecar`).
   - Lệnh sẽ thực thi trực tiếp qua `kubectl exec -c sidecar`.

3. **`--k8s-mode cp`:**
   - Biên dịch tĩnh binary `cmd/probe` và copy trực tiếp vào `/tmp/` của container ứng dụng qua `kubectl cp`.
   - Phù hợp khi chỉ cần đo Capacity Sizing sạch (Pha 1) hoặc cụm K8s bị tắt tính năng Ephemeral Containers.

---

## Các bước thực hiện của Agent:

### 1. Thu thập thông tin từ User / Ngữ cảnh
Xác định các tham số cần thiết:
- **`--url`**: URL dịch vụ cần test (ví dụ `http://127.0.0.1:8000/chat` hoặc Pod IP/ClusterIP).
- **`--scenario`**: Kịch bản kiểm thử (`chat-sse`, `sample-get`, `sample-post` hoặc custom).
- **`--steps`**: Dải bậc RPS (mặc định: `2,4,8,16,32`).
- **`--profile-type`**: `pyspy` (nếu app là Python/FastAPI/Uvicorn) hoặc `perf` (nếu là Go, C++, Rust, Java).
- **`--pod` & `--namespace`**: Tên Pod và Namespace mục tiêu trên K8s.
- **`--container`**: Tên container app trong Pod (dùng làm target cho debug container).
- **`--k8s-mode`**: `debug` (khuyên dùng), `sidecar`, hoặc `cp`.

### 2. Thực thi Script điều phối 2 Pha
Chạy script điều phối tích hợp sẵn trong skill:
```bash
./.agents/skills/capsizer-two-phase-profiling/scripts/run_two_phase.sh \
  --url "<URL>" \
  --scenario "<SCENARIO>" \
  --steps "2,4,8,16,32" \
  --profile-type "<pyspy|perf>" \
  --pod "<POD_NAME>" \
  --namespace "<NAMESPACE>" \
  --container "<APP_CONTAINER_NAME>" \
  --k8s-mode "debug"
```

### 3. Phân tích kết quả và phản hồi cho User
Sau khi script hoàn tất, đọc file báo cáo và trả về kết quả:

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
