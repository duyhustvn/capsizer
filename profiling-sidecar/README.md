# Profiling Sidecar & Debug Container

Container image chuyên dụng để **profiling, troubleshooting và trích xuất FlameGraph** cho các ứng dụng chạy trên Linux / Kubernetes Pod.

## 🧰 Các công cụ & Scripts tiện ích tích hợp sẵn

### Công cụ lõi
* **`py-spy`** (0.4.2): Sampling profiler tốc độ cao cho Python (không cần sửa code hay restart tiến trình).
* **`perf`** (`linux-tools-generic`): Profiler hệ thống Linux chuẩn (CPU cycles, cache misses, kernel & native call stacks).
* **`FlameGraph`** (Brendan Gregg): Bộ công cụ chuyển stack trace thành biểu đồ `.svg` trực quan (sẵn trong `PATH`: `flamegraph.pl`, `stackcollapse-perf.pl`,...).
* **`ss`** (`iproute2`): Tiện ích điều tra socket và thống kê kết nối mạng chi tiết.
* **`busybox`**: Bộ tiện ích dòng lệnh hỗ trợ debug nhanh.

### Scripts tiện ích (Bake sẵn trong `/usr/local/bin`)
* **`probe`**: Daemon thu thập chỉ số cgroup (CPU CFS quota/throttling, memory), TCP accept queue backlog, trạng thái socket, tiến trình worker và độ trễ HTTP health check theo chu kỳ (định dạng `.jsonl`), được viết bằng Go biên dịch binary tĩnh tối ưu hiệu năng.
* **`profile-pyspy`** (hoặc `profile-pyspy.sh`): Tự động quét tiến trình Python, đo CPU và render FlameGraph SVG trong 1 lệnh.
* **`profile-perf`** (hoặc `profile-perf.sh`): Chạy `perf record` + `flamegraph.pl` tự động với tần số 99Hz tối ưu.
* **`analyze-connections`** (hoặc `analyze-connections.sh`): Báo cáo tình trạng kết nối mạng, phát hiện rò rỉ socket (`CLOSE-WAIT`), nghẽn hàng đợi (`Recv-Q/Send-Q`), và Top Remote IPs.

---

## 🚀 Hướng dẫn sử dụng trên Kubernetes

Để profile tiến trình của một container khác trong cùng Pod, container debug cần:
1. **Chia sẻ Process Namespace** (`shareProcessNamespace: true` hoặc dùng `kubectl debug --target`).
2. **Quyền PTRACE** (`SYS_PTRACE` hoặc `SYS_ADMIN`) để đọc stack trace và bộ nhớ của tiến trình mục tiêu.

---

### Cách 1: Debug trực tiếp Pod đang chạy với `kubectl debug` (Khuyên dùng)

> **Ưu điểm:** Không cần cấu hình trước hay khởi động lại Pod.

#### 1. Đính kèm Ephemeral Container vào Pod mục tiêu
```bash
# Tham số --target chỉ định container chứa ứng dụng cần profile
kubectl debug -it <POD_NAME> -n <NAMESPACE> \
  --image=docker.io/duyle95/profiling-sidecar:latest \
  --target=<CONTAINER_NAME> \
  --profile=sysadmin \
  -- bash
```

> ⚠️ **Lưu ý khi Cluster bật Pod Security Standards (PSA/PSS) hoặc Kyverno / Gatekeeper:**
> Nếu Namespace enforce mức `baseline` hoặc `restricted`, lệnh trên có thể bị chặn vì `--profile=sysadmin` yêu cầu quyền `privileged`/`hostPID`. Bạn có 2 cách giải quyết:
>
> **Cách A: Dùng file cấu hình quyền tùy chỉnh (`--custom` trên K8s 1.27+)**
> Tạo file `debug-security.json` hoặc `.yaml` chỉ cấp đúng quyền `SYS_PTRACE` tối thiểu:
> ```yaml
> # debug-security.yaml
> securityContext:
>   capabilities:
>     add:
>       - SYS_PTRACE
>   runAsUser: 0
>   runAsNonRoot: false
>   allowPrivilegeEscalation: true
> ```
> Chạy lệnh:
> ```bash
> kubectl debug -it <POD_NAME> -n <NAMESPACE> \
>   --image=docker.io/duyle95/profiling-sidecar:latest \
>   --target=<CONTAINER_NAME> \
>   --custom=debug-security.yaml \
>   -- bash
> ```
>
> **Cách B: Sao chép Pod sang Pod Debug độc lập (`--copy-to`)**
> ```bash
> kubectl debug <POD_NAME> -n <NAMESPACE> \
>   --copy-to=<POD_NAME>-debug \
>   --share-processes \
>   --image=docker.io/duyle95/profiling-sidecar:latest \
>   -it -- bash
> ```


---

### Cách 2: Triển khai sẵn dưới dạng Sidecar Container trong Deployment

Khai báo trực tiếp `profiling-sidecar` trong manifest Pod hoặc Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      # BẮT BUỘC: Cho phép các container trong Pod thấy tiến trình của nhau
      shareProcessNamespace: true

      containers:
        # Container ứng dụng chính
        - name: app
          image: python:3.10-slim
          command: ["python", "-c", "import time; [time.sleep(1) for _ in iter(int, 1)]"]

        # Profiling Sidecar Container
        - name: profiling-sidecar
          image: docker.io/duyle95/profiling-sidecar:latest
          imagePullPolicy: IfNotPresent
          command: ["sh", "-c", "sleep infinity"]
          securityContext:
            capabilities:
              add:
                - SYS_PTRACE # Bắt buộc để inspect process
                - SYS_ADMIN  # Cần thiết nếu dùng Linux perf kernel events
```

Truy cập vào sidecar:
```bash
kubectl exec -it <POD_NAME> -c profiling-sidecar -n <NAMESPACE> -- bash
```

---

## 🔍 Các kịch bản Profiling & Xuất FlameGraph mẫu

Sau khi đã truy cập shell bên trong container debug:

### 1. Tìm PID của ứng dụng mục tiêu
```bash
ps aux
# Hoặc lọc theo tên:
pgrep -fl python
```

---

### 2. Profile ứng dụng Python với `py-spy`

> 💡 **Cách nhanh nhất: Sử dụng script `profile-pyspy` tích hợp sẵn**
> ```bash
> # Tự động quét tiến trình Python trong namespace và xuất file FlameGraph SVG:
> profile-pyspy
> 
> # Hoặc chỉ định PID, thời gian đo (giây), và kèm C/C++ extensions:
> profile-pyspy -p <PID> -d 60 --native
> 
> # Xem live CPU top theo từng dòng code:
> profile-pyspy --top -p <PID>
> ```

* **Các lệnh thủ công nâng cao:**
  * **Xem Live Top CPU:** `py-spy top --pid <PID>`
  * **In Call Stack hiện tại (Dump):** `py-spy dump --pid <PID>`
  * **Ghi FlameGraph thủ công:** `py-spy record --pid <PID> --duration 30 --output /tmp/python-flamegraph.svg`

---

### 3. Profile toàn hệ thống / Native C/C++ / Go / Rust bằng Linux `perf`

> 💡 **Cách nhanh nhất: Sử dụng script `profile-perf` tích hợp sẵn**
> ```bash
> # Profile một tiến trình trong 30s và tự động render FlameGraph SVG:
> profile-perf <PID>
> 
> # Profile ứng dụng Go / Rust / C++ tối ưu hóa (dùng call-graph dwarf):
> profile-perf -p <PID> -g dwarf -d 60
> 
> # Profile toàn bộ hệ thống (tất cả CPU trong namespace):
> profile-perf -a -d 15
> ```

#### 📌 Quy trình thủ công chi tiết (nếu không dùng script):

##### Bước 1: Thu thập dữ liệu Profiling (`perf record`)

Chạy lệnh ghi mẫu bên trong shell của container debug:
```bash
perf record -F 99 -p <PID> -g -o /tmp/perf.data -- sleep 30
```

**Giải thích các tùy chọn (Options) cơ bản:**
* **`-F 99`** (`--freq=99`): Tần số lấy mẫu (Sampling frequency) là 99 lần/giây trên mỗi CPU core. Chọn **99Hz** thay vì 100Hz để tránh lỗi *lockstep sampling* (lấy mẫu trùng nhịp với ngắt đồng hồ hệ thống định kỳ như 100Hz/1000Hz).
* **`-p <PID>`** (`--pid`): Chỉ định chính xác Process ID của tiến trình ứng dụng cần profile.
* **`-g`** (`--call-graph`): Kích hoạt thu thập Call Stack Trace (bắt buộc để dựng cây gọi hàm và vẽ FlameGraph).
* **`-o /tmp/perf.data`**: Đường dẫn file lưu trữ dữ liệu thô nhị phân.
* **`-- sleep 30`**: Lệnh thực thi đi kèm để ấn định khoảng thời gian profile đúng 30 giây. Khi `sleep` kết thúc, `perf` sẽ tự động dừng và lưu file.

**Các tùy chọn nâng cao (nếu muốn phân tích chi tiết hơn):**
* **`--call-graph dwarf`** (hoặc `-g --call-graph dwarf`): Dùng bảng gỡ lỗi DWARF để giải mã call stack sâu và chính xác. Cực kỳ cần thiết cho các ứng dụng **Go, Rust, C++** biên dịch tối ưu hóa bị bỏ Frame Pointer (`-fomit-frame-pointer`). *Lưu ý: File `perf.data` sẽ có dung lượng lớn hơn nhiều.*
* **`-a`** (`--all-cpus`, thay cho `-p <PID>`): Profile toàn bộ hệ thống (tất cả các tiến trình chạy trên mọi CPU core của worker node).
* **`-e <event>`**: Lọc theo loại sự kiện phần cứng/hệ thống cụ thể (mặc định là `cycles`). Các event phổ biến khác: `-e cpu-clock`, `-e instructions`, `-e cache-misses`, `-e page-faults`.

---

#### 📌 Bước 2: Xử lý dữ liệu & Xuất kết quả (Chọn 1 trong 2 cách)

Sau khi có file `/tmp/perf.data`, chọn 1 trong 2 cách xuất kết quả dưới đây:

##### 🌟 Cách A: Render trực tiếp file `.svg` trong Pod rồi copy về máy (Khuyên dùng)
> *Ưu điểm:* Container có sẵn Symbol Tables của tiến trình nên phân giải tên hàm chính xác nhất; file `.svg` sinh ra rất nhẹ (< 1MB), dễ copy và xem ngay trên mọi trình duyệt.

1. **Chuyển đổi dữ liệu và vẽ FlameGraph ngay trong Pod:**
   ```bash
   perf script -i /tmp/perf.data | stackcollapse-perf.pl | flamegraph.pl --title="CPU FlameGraph" > /tmp/perf-flamegraph.svg
   ```
   * **`perf script`**: Giải mã file nhị phân `perf.data` thành danh sách stack trace dạng văn bản (text).
   * **`stackcollapse-perf.pl`**: Gom nhóm (aggregate) các call stack trùng lặp thành định dạng 1 dòng (`func_a;func_b 120`).
   * **`flamegraph.pl`**: Đọc dữ liệu đã gom nhóm và sinh biểu đồ tương tác SVG.

2. **Copy file `.svg` về máy tính cá nhân (chạy từ terminal máy local):**
   ```bash
   # Nếu dùng Ephemeral Debug Container (kubectl debug):
   kubectl cp <NAMESPACE>/<POD_NAME>:/tmp/perf-flamegraph.svg ./perf-flamegraph.svg -c <DEBUGGER_CONTAINER_NAME>

   # Nếu dùng Sidecar Container:
   kubectl cp <NAMESPACE>/<POD_NAME>:/tmp/perf-flamegraph.svg ./perf-flamegraph.svg -c profiling-sidecar
   ```

3. **Mở xem biểu đồ:** Click đúp mở file `perf-flamegraph.svg` bằng trình duyệt web (Google Chrome, Firefox, Edge).

---

##### 💻 Cách B: Copy file thô `perf.data` về máy tính để phân tích chuyên sâu
> *Ưu điểm:* Giúp xem trực tiếp giao diện tương tác dòng lệnh `perf report` (TUI) trên máy cá nhân hoặc mở rộng phân tích bằng các công cụ local.

1. **Copy file `perf.data` về máy tính cá nhân (chạy từ terminal máy local):**
   ```bash
   kubectl cp <NAMESPACE>/<POD_NAME>:/tmp/perf.data ./perf.data -c <CONTAINER_NAME>
   ```

2. **Phân tích trên máy cá nhân (yêu cầu máy local có cài `perf`):**
   ```bash
   # Xem báo cáo tương tác TUI (dùng phím mũi tên duyệt hàm):
   perf report -i ./perf.data

   # Hoặc tự render FlameGraph trên máy local:
   perf script -i ./perf.data | stackcollapse-perf.pl | flamegraph.pl > local-flamegraph.svg
   ```

---

### 4. Phân tích kết nối mạng & Troubleshooting Socket với `analyze-connections`

Script `analyze-connections` sử dụng `ss` (Socket Statistics) để quét toàn diện tình trạng mạng trong namespace của Pod, tự động phát hiện rò rỉ socket, nghẽn hàng đợi buffer:

```bash
# 1. Báo cáo tổng quan toàn diện (Summary socket, TCP states breakdown, Queue backlog, Listening ports, Top IPs)
analyze-connections

# 2. Theo dõi kết nối thời gian thực (Live watch mode, tự động refresh mỗi 2s)
analyze-connections -w

# 3. Điều tra lỗi rò rỉ socket (Lọc các kết nối bị kẹt ở CLOSE-WAIT)
analyze-connections -s close-wait

# 4. Phát hiện các socket bị nghẽn hàng đợi (Recv-Q > 0 hoặc Send-Q > 0)
analyze-connections --queues

# 5. Lọc kết nối theo Port cụ thể (Local hoặc Remote port)
analyze-connections -p 8080

# 6. Xem Top 20 địa chỉ IP từ xa kết nối tới Pod
analyze-connections -t 20
```

---

### 5. Thu thập chỉ số Cgroup, TCP Accept Queue & Health Check với `probe`

Công cụ `probe` (Go binary tĩnh) liên tục thu thập viễn trắc hệ điều hành bên trong Pod theo chu kỳ (mặc định 1 giây), ghi ra file JSON Lines (`.jsonl`):
* **CPU Quota & CFS Throttling**: Số chu kỳ và thời gian bị bóp nghẽn (`nr_throttled`, `throttled_usec`), phát hiện thiếu CPU core.
* **TCP Socket & Accept Queue**: Hàng đợi kết nối (`accept_queue` / `backlog_max`) và phân bố trạng thái socket (`ESTAB`, `TIME_WAIT`, `SYN_RECV`,...).
* **Tiến trình ứng dụng**: Tự động quét các worker process (`uvicorn`, `gunicorn`, `granian` hoặc Go process PID 1) để đếm số FD và số OS threads.
* **HTTP Health Check Latency**: Đo thời gian phản hồi (ms) của endpoint `/health/live`.

```bash
# 1. Chạy thường trực lấy mẫu trong Pod:
probe --port 8000 --out /tmp/probe.jsonl

# 2. Lấy nhanh 1 snapshot metadata và hiện trạng container rồi thoát ngay:
probe --once

# 3. Tùy chỉnh chu kỳ lấy mẫu (ví dụ 0.5s) và URL endpoint health check:
probe --interval 0.5 --target http://127.0.0.1:8000 --health-timeout 2.0 --out /tmp/probe.jsonl

# 4. Chạy từ bên ngoài Pod (chỉ đo độ trễ health check, không đọc cgroup):
probe --target http://<POD_IP>:8000 --no-cgroup --out /tmp/probe-outside.jsonl
```

---

## 📥 Mẹo thao tác và tương tác với file FlameGraph `.svg`

Sau khi tải file SVG về máy tính và mở bằng trình duyệt:
* **Click vào một hàm:** Phóng to (zoom) sâu vào call stack của nhánh hàm đó.
* **Click `Reset Zoom` (góc trên bên trái):** Quay trở lại toàn bộ biểu đồ ban đầu.
* **Click `Search` (góc trên bên phải):** Nhập tên function/module/thư viện để highlight màu tím nổi bật trên biểu đồ.
* **Độ rộng của thanh:** Tương ứng với tỉ lệ thời gian chiếm dụng CPU (thanh càng rộng $\rightarrow$ hàm tiêu tốn càng nhiều tài nguyên).

---

## 🔒 Quy trình dọn dẹp sau khi debug để đảm bảo An toàn thông tin (ATTT)

Sau khi hoàn tất quá trình profiling/troubleshooting, cần thực hiện dọn dẹp để tránh rò rỉ dữ liệu nhạy cảm và vi phạm chính sách bảo mật (Security Compliance):

### 1. Dọn dẹp dữ liệu nhạy cảm trong Pod trước khi thoát
Dữ liệu profiling (`perf.data`, memory dump, stack trace) có thể lưu vết token, mật khẩu hoặc dữ liệu người dùng. Trước khi thoát container debug:
```bash
# Xóa toàn bộ file dump/trace tạm thời
rm -rf /tmp/*.svg /tmp/*.data /tmp/perf.data* /tmp/*.dump
exit
```

### 2. Xóa Container Debug / Khôi phục Pod nguyên bản

* **Nếu debug trực tiếp trên Pod thật (Ephemeral Container):**
  > **Lưu ý K8s:** Ephemeral container không thể xóa riêng lẻ khỏi Pod spec sau khi đã tạo. Khi thoát (`exit`), container chuyển sang trạng thái `Completed`/`Terminated`.
  > Để xóa hoàn toàn dấu vết container đặc quyền này nhằm tuân thủ audit ATTT:
  ```bash
  # Xóa Pod để ReplicaSet tự động sinh Pod mới sạch 100%:
  kubectl delete pod <POD_NAME> -n <NAMESPACE>

  # Hoặc restart rolling an toàn:
  kubectl rollout restart deployment/<DEPLOYMENT_NAME> -n <NAMESPACE>
  ```

* **Nếu debug bằng Pod bản sao (`--copy-to`):**
  ```bash
  # Xóa bỏ hoàn toàn Pod clone sau khi debug:
  kubectl delete pod <POD_NAME>-debug -n <NAMESPACE>
  ```

### 3. Thu hồi quyền tạm thời (nếu có)
* Xóa các `PolicyException` (Kyverno/OPA Gatekeeper) đã tạo tạm thời.
* Đưa nhãn Pod Security của Namespace về mức ban đầu (`baseline` hoặc `restricted`).

