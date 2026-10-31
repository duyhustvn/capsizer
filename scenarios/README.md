# HƯỚNG DẪN VIẾT KỊCH BẢN KIỂM THỬ DECLARATIVE & GENERIC DATASETS

Capsizer hỗ trợ cơ chế kịch bản **Declarative JSON** kết hợp **Generic Datasets (Data Feeders)**, giúp người dùng tự định nghĩa và chạy các bài kiểm thử tải hoặc profiling trên bất kỳ máy nào sau khi cài gói `.deb` (`capsizer_1.0.0_amd64.deb`) mà **không cần cài đặt Go SDK hay biên dịch lại mã nguồn**.

---

## 1. Cấu trúc thư mục một kịch bản

Mỗi kịch bản được đóng gói trong một thư mục riêng biệt:

```text
scenarios/my-api-test/
├── scenario.json       # [Bắt buộc] Khai báo chi tiết request, headers, body, datasets, logic SSE/REST
├── scenario.env        # [Tùy chọn] Cấu hình bậc tải (STEPS), hạ tầng profiling (K8S_SELECTOR, PROFILE_TYPE)
├── payload.json        # [Tùy chọn] File template body payload nếu không viết inline trong JSON
├── queries.txt         # [Tùy chọn] File dữ liệu dòng (Text lines)
├── users.csv           # [Tùy chọn] File dữ liệu người dùng (CSV)
└── products.jsonl      # [Tùy chọn] File dữ liệu phức tạp (JSON Lines)
```

---

## 2. Cấu trúc file `scenario.json`

| Trường | Kiểu | Mô tả |
| :--- | :--- | :--- |
| `name` | `string` | Tên kịch bản (ví dụ: `"sample-post"`, `"chat-sse"`, `"ecommerce-checkout"`) |
| `type` | `string` | Kiểu giao thức: `"http"` (REST API) hoặc `"sse"` (Streaming LLM Chatbot) |
| `url` | `string` | Endpoint nhận request (hỗ trợ template `{{.items.id}}`, `{{.UUID}}`,...) |
| `method` | `string` | Phương thức HTTP: `"GET"`, `"POST"`, `"PUT"`, `"DELETE"`,... |
| `headers` | `map[string]string` | HTTP headers (hỗ trợ template `Authorization: Bearer {{.users.token}}`) |
| `body` | `string` | Chuỗi nội dung JSON/text template gửi trong body |
| `body_file` | `string` | Đường dẫn file body payload template (thay thế trường `body`) |
| `datasets` | `map[string]DatasetConfig` | Khai báo các nguồn nạp dữ liệu động (Generic Datasets / Feeders) |
| `expected_status` | `[]int` | Danh sách mã HTTP hợp lệ (mặc định: `[200, 201]`) |
| `timeout_seconds` | `float` | Timeout tối đa cho mỗi request (giây, mặc định 120s) |
| `cache_bust` | `bool` | Tự động thêm chuỗi timestamp ngẫu nhiên chống API Gateway/CDN cache |
| `sse.end_marker` | `string` | Chuỗi ký hiệu kết thúc stream SSE (mặc định: `"[DONE]"`) |

---

## 3. Cơ chế Generic Datasets (Data Feeders)

Thay vì cố định cứng các file dữ liệu, bạn có thể khai báo bất kỳ số lượng dataset nào dưới mục `datasets`:

```json
"datasets": {
  "users": {
    "file": "users.csv",
    "mode": "sequential"
  },
  "prompts": {
    "file": "queries.txt",
    "mode": "random"
  },
  "items": {
    "file": "items.jsonl",
    "mode": "sequential"
  }
}
```

### 3.1. Các định dạng dữ liệu hỗ trợ (Formats)

Hệ thống tự động nhận diện định dạng dựa vào phần mở rộng của file (hoặc cấu hình tường minh qua trường `"format"`):

| Định dạng | Phần mở rộng | Mô tả & Cách truy xuất trong Template |
| :--- | :--- | :--- |
| **CSV** | `.csv` | Hàng 1 làm Header. Mỗi bản ghi là map các cột: `{{.users.token}}`, `{{.users.user_id}}`, `{{.users.tier}}`. Tự động chuẩn hóa chữ hoa/chữ thường. |
| **Lines / Txt** | `.txt` | Mỗi dòng là một bản ghi văn bản. Truy xuất trực tiếp bằng `{{.prompts}}` hoặc `{{.prompts.Line}}`. |
| **JSON Lines** | `.jsonl`, `.ndjson` | Mỗi dòng là 1 object JSON hợp lệ. Truy xuất trường lồng nhau: `{{.items.sku}}`, `{{.items.price}}`. |
| **JSON Array/Object** | `.json` | Nạp file JSON (mảng các object hoặc 1 object). Truy xuất: `{{.clusters.name}}`. |

### 3.2. Chế độ cấp dữ liệu (Modes)

- `"random"` *(Mặc định)*: Chọn ngẫu nhiên một bản ghi trong tập dữ liệu cho mỗi request. Phù hợp cho câu hỏi prompt, từ khóa tìm kiếm, sản phẩm ngẫu nhiên.
- `"sequential"`: Cấp phát tuần tự xoay vòng (Round-Robin) lần lượt qua danh sách bản ghi, an toàn tuyệt đối đa luồng (`atomic.AddUint64`, lock-free 100%). Phù hợp cho danh sách Account/Token người dùng, đơn hàng cần đi theo trình tự.

### 3.3. Hỗ trợ đường dẫn File linh hoạt (Relative & Absolute Path)

Trường `"file"` trong `datasets` và trường `"body_file"` hỗ trợ toàn diện tất cả các kiểu đường dẫn:

1. **Đường dẫn tương đối theo thư mục kịch bản (Scenario-relative)**:
   - `"file": "users.csv"`: Tìm file nằm cùng thư mục với `scenario.json`.
   - `"file": "subdata/items.jsonl"`: Tìm file trong thư mục con.
   - `"file": "../shared/tokens.csv"`: Tìm file ở thư mục cha hoặc thư mục dùng chung giữa các scenarios.
2. **Đường dẫn tương đối theo thư mục làm việc (Terminal Working Directory)**:
   - Khi ghi đè qua CLI: `-opt dataset.users.file=./local_users.csv`, file được tìm kiếm theo thư mục bạn đang đứng gõ lệnh.
3. **Đường dẫn tuyệt đối (Absolute path)**:
   - `"file": "/var/data/users.csv"` hoặc `"file": "/data/test/prompts.txt"`.
4. **Ký tự thư mục Home (`~`)**:
   - `"file": "~/data/prompts.txt"`: Tự động phân giải thành thư mục home của người dùng hiện tại.
5. **Biến môi trường hệ thống (`$ENV` / `${ENV}`)**:
   - `"file": "$DATA_DIR/users.csv"` hoặc `"file": "${SHARED_PATH}/items.jsonl"`.

---

## 4. Các biến Template và Helper Functions có sẵn

### 4.1. Biến động hệ thống

- `{{.UUID}}`: Chuỗi UUID ngẫu nhiên duy nhất cho mỗi request (thường dùng làm `X-Request-ID`).
- `{{.RandomInt}}`: Số nguyên ngẫu nhiên từ 100,000 đến 999,999.
- `{{.Timestamp}}`: Thời gian epoch Unix tính bằng giây.
- `{{.TimestampNano}}`: Thời gian epoch Unix tính bằng nano-giây.

### 4.2. Helper Functions tiện ích

- `{{quote .val}}`: JSON-quote và escape an toàn chuỗi văn bản (ví dụ nếu prompt chứa dấu ngoặc kép `"` hoặc xuống dòng, `quote` sẽ tự động thoát chuỗi thành chuỗi JSON hợp lệ).
- `{{json .obj}}`: Chuyển đổi đối tượng bất kỳ thành chuỗi JSON.
- `{{lower .str}}`: Chuyển chuỗi thành chữ thường.
- `{{upper .str}}`: Chuyển chuỗi thành chữ hoa.
- `{{trim .str}}`: Cắt tỉa khoảng trắng ở hai đầu.

---

## 5. Ví dụ mẫu

### Ví dụ 1: Chatbot AI Streaming (Server-Sent Events)
Sử dụng dataset `prompts` ngẫu nhiên và `users` tuần tự:

```json
{
  "name": "chat-sse",
  "type": "sse",
  "url": "http://10.0.0.10:8000/chat",
  "method": "POST",
  "headers": {
    "Content-Type": "application/json",
    "Authorization": "Bearer {{.users.token}}"
  },
  "body": "{\"prompt\": {{quote .prompts}}, \"stream\": true}",
  "datasets": {
    "prompts": {
      "file": "queries.txt",
      "mode": "random"
    },
    "users": {
      "file": "users.csv",
      "mode": "sequential"
    }
  },
  "expected_status": [200],
  "sse": {
    "end_marker": "[DONE]"
  }
}
```

### Ví dụ 2: E-commerce Order API với nhiều Datasets
Kết hợp `products.csv` và `tenants.jsonl`:

```json
{
  "name": "ecommerce-order",
  "type": "http",
  "url": "https://api.internal/v1/tenants/{{.tenants.org_id}}/orders",
  "method": "POST",
  "headers": {
    "Content-Type": "application/json",
    "X-Request-ID": "{{.UUID}}",
    "X-Tenant-Key": "{{.tenants.api_key}}"
  },
  "body": "{\"sku\": \"{{.products.sku}}\", \"quantity\": {{.RandomInt}}, \"unit_price\": {{.products.price}}}",
  "datasets": {
    "products": {
      "file": "products.csv",
      "mode": "random"
    },
    "tenants": {
      "file": "tenants.jsonl",
      "mode": "sequential"
    }
  },
  "expected_status": [200, 201]
}
```

---

## 6. Ghi đè Datasets từ dòng lệnh (CLI Overrides)

Người dùng có thể ghi đè đường dẫn file hoặc chế độ nạp của bất kỳ dataset nào khi chạy lệnh mà không cần sửa file JSON:

```bash
# Ghi đè file của dataset 'prompts'
capsizer-ramp -scenario scenarios/chatbot -opt dataset.prompts.file=/data/new_prompts.txt

# Ghi đè chế độ của dataset 'prompts' sang tuần tự
capsizer-ramp -scenario scenarios/chatbot -opt dataset.prompts.mode=sequential

# Ghi đè tắt nhanh bằng tên dataset
capsizer-ramp -scenario scenarios/chatbot -opt prompts=/data/custom_prompts.txt

# Bổ sung một dataset mới hoàn toàn từ CLI
capsizer-ramp -scenario scenarios/chatbot -opt dataset.extras.file=/data/extras.csv
```

---

## 7. Cách chạy kiểm thử với gói `.deb`

Sau khi cài gói `sudo dpkg -i capsizer_1.0.0_amd64.deb` trên máy bắn tải:

```bash
# Cách 1: Chạy bằng tên thư mục kịch bản
capsizer-ramp -scenario ./scenarios/chatbot -steps 10,20,40,80 -step-seconds 30

# Cách 2: Chạy trực tiếp file scenario.json
capsizer-ramp -scenario /opt/my-scenario/scenario.json -steps 5,10,15

# Cách 3: Ghi đè URL mục tiêu và file dataset từ command line
capsizer-ramp -scenario ./scenarios/chatbot \
  -url http://staging.internal:8000/chat \
  -opt prompts=/tmp/test_prompts.txt \
  -steps 10,20
```
