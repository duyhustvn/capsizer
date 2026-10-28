# HƯỚNG DẪN VIẾT KỊCH BẢN KIỂM THỬ DECLARATIVE (TƯƠNG TỰ K6)

Capsizer hỗ trợ cơ chế kịch bản **Declarative JSON**, giúp người dùng tự định nghĩa và chạy các bài kiểm thử tải hoặc profiling trên bất kỳ máy nào sau khi cài gói `.deb` (`capsizer_1.0.0_amd64.deb`) mà **không cần cài đặt Go SDK hay biên dịch lại mã nguồn**.

---

## 1. Cấu trúc thư mục một kịch bản

Mỗi kịch bản thường được đặt trong một thư mục riêng biệt:

```text
scenarios/my-api-test/
├── scenario.json       # [Bắt buộc] Khai báo chi tiết request, headers, body, logic SSE/REST
├── scenario.env        # [Tùy chọn] Cấu hình bậc tải (STEPS), hạ tầng (K8S_SELECTOR, PROFILE_TYPE)
├── payload.json        # [Tùy chọn] File template body payload
├── queries.txt         # [Tùy chọn] Danh sách câu hỏi/prompts phát sinh động
└── users.csv           # [Tùy chọn] Danh bạ tokens/users
```

---

## 2. Cấu trúc file `scenario.json`

| Trường | Kiểu | Mô tả |
| :--- | :--- | :--- |
| `name` | `string` | Tên kịch bản (ví dụ: `"sample-post"`, `"llm-chat"`) |
| `type` | `string` | Kiểu giao thức: `"http"` (REST API) hoặc `"sse"` (Streaming LLM Chatbot) |
| `url` | `string` | Endpoint nhận request (hỗ trợ template `{{.Query}}`, `{{.UUID}}`,...) |
| `method` | `string` | Phương thức HTTP: `"GET"`, `"POST"`, `"PUT"`, `"DELETE"`,... |
| `headers` | `map[string]string` | HTTP headers (hỗ trợ template `Authorization: Bearer {{.Token}}`) |
| `body` | `string` | Chuỗi nội dung JSON/text template gửi trong body |
| `body_file` | `string` | Đường dẫn file body payload template (thay thế trường `body`) |
| `queries_file` | `string` | File chứa danh sách câu hỏi / prompt (mỗi dòng 1 câu) |
| `users_file` | `string` | File CSV chứa danh bạ token người dùng (`token,id,...`) |
| `expected_status` | `[]int` | Danh sách mã HTTP hợp lệ (mặc định: `[200, 201]`) |
| `timeout_seconds` | `float` | Timeout tối đa cho mỗi request (giây) |
| `cache_bust` | `bool` | Tự động thêm query `_cb=<timestamp>` để ngăn CDN/API Gateway cache |
| `sse.end_marker` | `string` | Chuỗi ký hiệu kết thúc stream (mặc định: `"[DONE]"`) |

---

## 3. Các biến Template có sẵn trong Header, Body và URL

Mỗi khi một request được sinh ra theo mô hình Open-Loop, Capsizer tự động thay thế các biến sau:

- `{{.Query}}`: Lấy ngẫu nhiên 1 dòng từ `queries_file`.
- `{{.Token}}`: Token xác thực người dùng lấy từ `users_file`.
- `{{.UserID}}`: ID người dùng tương ứng từ `users_file`.
- `{{.UUID}}`: Chuỗi UUID ngẫu nhiên duy nhất cho mỗi request (thường dùng làm `X-Request-ID`).
- `{{.RandomInt}}`: Số nguyên ngẫu nhiên (từ 0 đến 1,000,000).
- `{{.Timestamp}}`: Thời gian epoch Unix tính bằng nano-giây.

---

## 4. Ví dụ mẫu

### Ví dụ 1: REST API POST có Header & Dynamic Payload
```json
{
  "name": "sample-post",
  "type": "http",
  "url": "https://jsonplaceholder.typicode.com/posts",
  "method": "POST",
  "headers": {
    "Content-Type": "application/json",
    "Accept": "application/json",
    "X-Request-ID": "{{.UUID}}"
  },
  "body_file": "payload.json",
  "expected_status": [200, 201]
}
```

### Ví dụ 2: Chatbot AI Streaming (Server-Sent Events)
```json
{
  "name": "chatbot-sse",
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
  "expected_status": [200],
  "sse": {
    "end_marker": "[DONE]"
  }
}
```

---

## 5. Cách chạy kiểm thử với gói `.deb`

Sau khi cài gói `sudo dpkg -i capsizer_1.0.0_amd64.deb` trên máy bắn tải:

```bash
# Cách 1: Truyền trực tiếp đường dẫn thư mục kịch bản
capsizer-ramp -scenario ./scenarios/my-api-test -steps 10,20,40,80 -step-seconds 30

# Cách 2: Truyền file scenario.json cụ thể
capsizer-ramp -scenario /opt/my-test/scenario.json -steps 5,10,15

# Cách 3: Ghi đè URL hoặc tham số từ command line
capsizer-ramp -scenario ./scenarios/my-api-test -url http://staging.internal:8080/api -steps 10,20
```
