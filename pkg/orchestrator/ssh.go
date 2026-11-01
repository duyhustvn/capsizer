package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// SSHClient bọc kết nối SSH tái sử dụng (Session Multiplexing) tới một máy từ xa.
type SSHClient struct {
	host       string
	user       string
	port       int
	keyPath    string
	client     *ssh.Client
	mu         sync.Mutex
	sshConfig  *ssh.ClientConfig
}

// NewSSHClient khởi tạo client SSH mới.
func NewSSHClient(user, host string, port int, keyPath string) (*SSHClient, error) {
	if port <= 0 {
		port = 22
	}

	authMethods := []ssh.AuthMethod{}

	// 1. Thử nạp private key nếu có đường dẫn
	if keyPath != "" {
		expandedKey := expandHome(keyPath)
		keyBytes, err := os.ReadFile(expandedKey)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(keyBytes)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}

	// 2. Thử các key mặc định trong ~/.ssh nếu chưa có key
	if len(authMethods) == 0 {
		home, _ := os.UserHomeDir()
		defaultKeys := []string{
			filepath.Join(home, ".ssh", "id_rsa"),
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_ecdsa"),
		}
		for _, dk := range defaultKeys {
			keyBytes, err := os.ReadFile(dk)
			if err == nil {
				signer, err := ssh.ParsePrivateKey(keyBytes)
				if err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	// 3. Thử SSH Agent nếu có
	if authSock := os.Getenv("SSH_AUTH_SOCK"); authSock != "" {
		if conn, err := net.Dial("unix", authSock); err == nil {
			agentClient := agent.NewClient(conn)
			authMethods = append(authMethods, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("không tìm thấy SSH key hợp lệ cho %s@%s", user, host)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	return &SSHClient{
		host:      host,
		user:      user,
		port:      port,
		keyPath:   keyPath,
		sshConfig: config,
	}, nil
}

// Connect thiết lập kết nối SSH nếu chưa có.
func (c *SSHClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return nil
	}

	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	client, err := ssh.Dial("tcp", addr, c.sshConfig)
	if err != nil {
		return fmt.Errorf("không thể kết nối SSH tới %s: %w", addr, err)
	}
	c.client = client
	return nil
}

// Close đóng kết nối SSH.
func (c *SSHClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// RunCommand thực thi một lệnh trên máy từ xa và trả về stdout kết hợp.
func (c *SSHClient) RunCommand(ctx context.Context, cmd string) (string, error) {
	if err := c.Connect(); err != nil {
		return "", err
	}

	session, err := c.client.NewSession()
	if err != nil {
		// Thử kết nối lại nếu session bị đứt
		c.Close()
		if err := c.Connect(); err != nil {
			return "", err
		}
		session, err = c.client.NewSession()
		if err != nil {
			return "", fmt.Errorf("không thể tạo SSH session: %w", err)
		}
	}
	defer session.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()
	defer close(done)

	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("lệnh thất bại: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// UploadFile copy file từ local lên remote qua SCP protocol stream.
func (c *SSHClient) UploadFile(ctx context.Context, localPath, remotePath string) error {
	if err := c.Connect(); err != nil {
		return err
	}

	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("không thể mở file local %s: %w", localPath, err)
	}
	defer localFile.Close()

	stat, err := localFile.Stat()
	if err != nil {
		return err
	}

	session, err := c.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	go func() {
		<-ctx.Done()
		session.Close()
	}()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}

	dir := filepath.Dir(remotePath)
	base := filepath.Base(remotePath)

	// Chạy scp sink trên remote
	cmd := fmt.Sprintf("scp -t %s", dir)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("không thể khởi chạy scp sink trên remote: %w", err)
	}

	// Gửi SCP header: C<mode> <size> <filename>\n
	mode := fmt.Sprintf("%04o", stat.Mode().Perm())
	fmt.Fprintf(stdin, "C%s %d %s\n", mode, stat.Size(), base)

	if _, err := io.Copy(stdin, localFile); err != nil {
		return fmt.Errorf("lỗi copy dữ liệu lên remote: %w", err)
	}
	fmt.Fprint(stdin, "\x00")
	_ = stdin.Close()

	return session.Wait()
}

// DownloadFile copy file từ remote về local.
func (c *SSHClient) DownloadFile(ctx context.Context, remotePath, localPath string) error {
	if err := c.Connect(); err != nil {
		return err
	}

	session, err := c.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	go func() {
		<-ctx.Done()
		session.Close()
	}()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}

	cmd := fmt.Sprintf("scp -f %s", remotePath)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("không thể khởi chạy scp source trên remote: %w", err)
	}

	// Xác nhận bắt đầu nhận
	fmt.Fprint(stdin, "\x00")

	// Đọc header SCP: C<mode> <size> <filename>\n
	var mode, filename string
	var size int64
	var code byte
	if _, err := fmt.Fscanf(stdout, "%c", &code); err != nil {
		return fmt.Errorf("lỗi đọc SCP response: %w", err)
	}
	if code != 'C' {
		return fmt.Errorf("lỗi giao thức SCP: kỳ vọng 'C', nhận '%c'", code)
	}
	if _, err := fmt.Fscanf(stdout, "%s %d %s\n", &mode, &size, &filename); err != nil {
		return fmt.Errorf("lỗi đọc header SCP: %w", err)
	}

	// Xác nhận sẵn sàng nhận nội dung
	fmt.Fprint(stdin, "\x00")

	_ = os.MkdirAll(filepath.Dir(localPath), 0755)
	outFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("không thể tạo file local %s: %w", localPath, err)
	}
	defer outFile.Close()

	if _, err := io.CopyN(outFile, stdout, size); err != nil {
		return fmt.Errorf("lỗi đọc nội dung file từ remote: %w", err)
	}

	// Xác nhận hoàn tất
	fmt.Fprint(stdin, "\x00")
	_ = stdin.Close()

	return session.Wait()
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
