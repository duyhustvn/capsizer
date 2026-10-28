package scenarios

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// StrPtr trả về con trỏ tới chuỗi.
func StrPtr(s string) *string {
	return &s
}

// LoadDotEnv tìm và nạp các biến môi trường từ file .env vào process (os.Setenv) nếu chưa tồn tại.
func LoadDotEnv(path string) map[string]string {
	var candidates []string
	if path != "" {
		candidates = append(candidates, path)
		if !filepath.IsAbs(path) {
			candidates = append(candidates,
				filepath.Join(".", path),
				filepath.Join("..", path),
				filepath.Join("..", "..", path),
			)
		}
	} else {
		candidates = append(candidates,
			".env",
			filepath.Join("..", ".env"),
			filepath.Join("..", "..", ".env"),
		)
	}

	var targetPath string
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			targetPath = c
			break
		}
	}
	if targetPath == "" {
		return nil
	}

	content, err := os.ReadFile(targetPath)
	if err != nil {
		return nil
	}

	envVars := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		i++
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])

		// Xử lý chuỗi multiline nằm trong dấu ngoặc kép hoặc đơn
		if (strings.HasPrefix(v, "\"") && !(strings.HasSuffix(v, "\"") && len(v) > 1)) ||
			(strings.HasPrefix(v, "'") && !(strings.HasSuffix(v, "'") && len(v) > 1)) {
			quoteChar := string(v[0])
			valParts := []string{v[1:]}
			for i < len(lines) {
				nextLine := lines[i]
				i++
				if strings.HasSuffix(strings.TrimRight(nextLine, "\r"), quoteChar) {
					trimmed := strings.TrimRight(nextLine, "\r")
					valParts = append(valParts, trimmed[:len(trimmed)-1])
					break
				}
				valParts = append(valParts, strings.TrimRight(nextLine, "\r"))
			}
			v = strings.Join(valParts, "\n")
		} else if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
			(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
			v = v[1 : len(v)-1]
		}

		v = strings.ReplaceAll(v, "\\n", "\n")
		envVars[k] = v
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
	return envVars
}

// ParseQueries phân tích chuỗi câu hỏi từ .env (hỗ trợ JSON array, xuống dòng, | hoặc ;).
func ParseQueries(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// 1. JSON Array
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			var res []string
			for _, item := range list {
				item = strings.TrimSpace(item)
				if item != "" {
					res = append(res, item)
				}
			}
			if len(res) > 0 {
				return res
			}
		}
	}

	// 2. Phân tách theo dòng (\n)
	if strings.Contains(raw, "\n") {
		var res []string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				res = append(res, line)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	// 3. Phân tách theo dấu pipe (|)
	if strings.Contains(raw, "|") {
		var res []string
		for _, item := range strings.Split(raw, "|") {
			item = strings.TrimSpace(item)
			if item != "" {
				res = append(res, item)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	// 4. Phân tách theo dấu chấm phẩy (;)
	if strings.Contains(raw, ";") {
		var res []string
		for _, item := range strings.Split(raw, ";") {
			item = strings.TrimSpace(item)
			if item != "" {
				res = append(res, item)
			}
		}
		if len(res) > 0 {
			return res
		}
	}

	return []string{raw}
}

// ResolveQueries xác định danh sách câu hỏi kiểm thử theo độ ưu tiên:
// 1. Cờ dòng lệnh --queries (file txt/csv)
// 2. Biến CHAT_QUERIES_FILE hoặc QUERIES_FILE từ .env
// 3. Biến CHAT_QUERIES hoặc QUERIES từ .env
func ResolveQueries(queriesFile string, envFile string) []string {
	LoadDotEnv(envFile)

	// 1. Cờ dòng lệnh --queries
	if queriesFile != "" {
		content, err := os.ReadFile(queriesFile)
		if err == nil {
			var lines []string
			for _, line := range strings.Split(string(content), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					lines = append(lines, line)
				}
			}
			if len(lines) > 0 {
				fmt.Printf("queries  : %d câu hỏi nạp từ cờ --queries (%s)\n", len(lines), filepath.Base(queriesFile))
				return lines
			}
		}
		fmt.Fprintf(os.Stderr, "CẢNH BÁO: không đọc được file --queries: %s\n", queriesFile)
	}

	// 2. Biến file từ .env
	qFile := os.Getenv("CHAT_QUERIES_FILE")
	if qFile == "" {
		qFile = os.Getenv("QUERIES_FILE")
	}
	if qFile != "" {
		candidates := []string{
			qFile,
			filepath.Join("..", qFile),
			filepath.Join("..", "..", qFile),
		}
		for _, c := range candidates {
			content, err := os.ReadFile(c)
			if err == nil {
				var lines []string
				for _, line := range strings.Split(string(content), "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						lines = append(lines, line)
					}
				}
				if len(lines) > 0 {
					fmt.Printf("queries  : %d câu hỏi nạp từ file %s (qua .env)\n", len(lines), filepath.Base(c))
					return lines
				}
			}
		}
	}

	// 3. Chuỗi danh sách câu hỏi trong CHAT_QUERIES hoặc QUERIES (.env)
	rawEnv := os.Getenv("CHAT_QUERIES")
	if rawEnv == "" {
		rawEnv = os.Getenv("QUERIES")
	}
	if rawEnv != "" {
		parsed := ParseQueries(rawEnv)
		if len(parsed) > 0 {
			fmt.Printf("queries  : %d câu hỏi nạp từ .env (CHAT_QUERIES)\n", len(parsed))
			return parsed
		}
	}

	// Không tìm thấy câu hỏi từ cờ dòng lệnh hoặc cấu hình .env
	return nil
}


type User struct {
	ID    string
	Token string
}

type UserPool struct {
	users []User
	idx   uint64
}

func NewUserPool(users []User) *UserPool {
	return &UserPool{users: users}
}

func (p *UserPool) Len() int {
	return len(p.users)
}

func (p *UserPool) Next() (string, string) {
	if len(p.users) == 0 {
		return "", ""
	}
	i := atomic.AddUint64(&p.idx, 1) - 1
	u := p.users[i%uint64(len(p.users))]
	return u.ID, u.Token
}

// LoadUsers nạp danh sách user từ file CSV, nhận diện linh hoạt các cột token và user_id.
func LoadUsers(csvPath string) ([]User, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%s: file rỗng", csvPath)
	}

	header := records[0]
	tokenCol := -1
	userCol := -1
	for i, h := range header {
		clean := strings.ToLower(strings.TrimSpace(h))
		if clean == "token" || clean == "user_token" || clean == "access_token" || clean == "api_key" {
			if tokenCol == -1 {
				tokenCol = i
			}
		}
		if clean == "user_id" || clean == "userid" || clean == "id" || clean == "user" {
			if userCol == -1 {
				userCol = i
			}
		}
	}

	// Lượt 2: Tìm kiếm gần đúng (fuzzy match)
	if tokenCol == -1 {
		for i, h := range header {
			if strings.Contains(strings.ToLower(strings.TrimSpace(h)), "token") {
				tokenCol = i
				break
			}
		}
	}

	if tokenCol == -1 {
		return nil, fmt.Errorf("%s: không tìm thấy cột nào chứa token trong header %v", csvPath, header)
	}

	var users []User
	for i, row := range records[1:] {
		if tokenCol >= len(row) {
			continue
		}
		tok := strings.TrimSpace(row[tokenCol])
		if tok == "" {
			continue
		}
		uid := fmt.Sprintf("%d", i+1)
		if userCol != -1 && userCol < len(row) {
			val := strings.TrimSpace(row[userCol])
			if val != "" {
				uid = val
			}
		}
		users = append(users, User{ID: uid, Token: tok})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%s: không có dòng nào hợp lệ", csvPath)
	}
	return users, nil
}
