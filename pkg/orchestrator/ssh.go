package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SSHClient điều phối kết nối SSH/SCP qua OpenSSH hệ thống.
// Hỗ trợ đầy đủ ~/.ssh/config (Host alias, HostName, User, Port, IdentityFile, ProxyJump, ProxyCommand)
// và tái sử dụng kết nối qua OpenSSH ControlMaster.
type SSHClient struct {
	host        string
	user        string
	port        int
	keyPath     string
	jumpHost    string
	controlPath string
}

// NewSSHClient khởi tạo client SSH hỗ trợ ~/.ssh/config và ProxyJump.
func NewSSHClient(user, host string, port int, keyPath string, jumpHost ...string) (*SSHClient, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("thiếu thông tin SSH host")
	}

	jh := ""
	if len(jumpHost) > 0 {
		jh = strings.TrimSpace(jumpHost[0])
	}

	// Tạo đường dẫn socket ControlMaster ngắn gọn, duy nhất cho tổ hợp kết nối
	hashInput := fmt.Sprintf("%s|%s|%d|%s|%s", user, host, port, keyPath, jh)
	sum := sha1.Sum([]byte(hashInput))
	controlPath := filepath.Join(os.TempDir(), fmt.Sprintf("capsizer-ssh-%x", sum[:6]))

	return &SSHClient{
		host:        host,
		user:        strings.TrimSpace(user),
		port:        port,
		keyPath:     expandHome(strings.TrimSpace(keyPath)),
		jumpHost:    jh,
		controlPath: controlPath,
	}, nil
}

// DisplayTarget trả về chuỗi mô tả đích kết nối SSH thân thiện cho log/banner.
func (c *SSHClient) DisplayTarget() string {
	target := c.host
	if c.user != "" && !strings.Contains(target, "@") {
		target = c.user + "@" + target
	}
	if c.port > 0 {
		target = fmt.Sprintf("%s:%d", target, c.port)
	}
	if c.jumpHost != "" {
		target = fmt.Sprintf("%s (jump: %s)", target, c.jumpHost)
	}
	return target
}

func (c *SSHClient) commonOpts() []string {
	opts := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + c.controlPath,
		"-o", "ControlPersist=5m",
	}
	if c.keyPath != "" {
		opts = append(opts, "-i", c.keyPath)
	}
	if c.jumpHost != "" {
		opts = append(opts, "-J", c.jumpHost)
	}
	return opts
}

func (c *SSHClient) sshArgs(remoteCmd string) []string {
	args := c.commonOpts()
	if c.port > 0 {
		args = append(args, "-p", strconv.Itoa(c.port))
	}
	if c.user != "" && !strings.Contains(c.host, "@") {
		args = append(args, "-l", c.user)
	}
	args = append(args, c.host, remoteCmd)
	return args
}

func (c *SSHClient) scpBaseArgs() []string {
	args := c.commonOpts()
	if c.port > 0 {
		args = append(args, "-P", strconv.Itoa(c.port))
	}
	return args
}

func (c *SSHClient) scpHostPrefix() string {
	if c.user != "" && !strings.Contains(c.host, "@") {
		return fmt.Sprintf("%s@%s", c.user, c.host)
	}
	return c.host
}

// Connect kiểm tra kết nối SSH (và khởi tạo ControlMaster socket).
func (c *SSHClient) Connect() error {
	_, err := c.RunCommand(context.Background(), "true")
	return err
}

// Close đóng kết nối ControlMaster nếu đang mở.
func (c *SSHClient) Close() error {
	if c.controlPath == "" {
		return nil
	}
	args := []string{"-O", "exit", "-o", "ControlPath=" + c.controlPath, c.host}
	_ = exec.Command("ssh", args...).Run()
	return nil
}

// RunCommand thực thi một lệnh trên máy từ xa qua OpenSSH (tự động đọc ~/.ssh/config).
func (c *SSHClient) RunCommand(ctx context.Context, cmd string) (string, error) {
	args := c.sshArgs(cmd)
	execCmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("ssh [%s] thất bại: %w (output: %s)",
			c.DisplayTarget(), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// StreamCommand thực thi một lệnh trên máy từ xa qua OpenSSH và đọc từng dòng stdout theo thời gian thực.
func (c *SSHClient) StreamCommand(ctx context.Context, cmd string, onLine func(string)) error {
	args := c.sshArgs(cmd)
	execCmd := exec.CommandContext(ctx, "ssh", args...)

	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	execCmd.Stderr = &stderr

	if err := execCmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if onLine != nil {
			onLine(scanner.Text())
		}
	}

	waitErr := execCmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("ssh stream [%s] kết thúc với lỗi: %w (%s)",
			c.DisplayTarget(), waitErr, strings.TrimSpace(stderr.String()))
	}
	return scanner.Err()
}

// UploadFile copy file từ local lên remote qua scp (hỗ trợ ~/.ssh/config và ProxyJump).
func (c *SSHClient) UploadFile(ctx context.Context, localPath, remotePath string) error {
	args := c.scpBaseArgs()
	dst := fmt.Sprintf("%s:%s", c.scpHostPrefix(), remotePath)
	args = append(args, localPath, dst)

	execCmd := exec.CommandContext(ctx, "scp", args...)
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp upload [%s -> %s] thất bại: %w (%s)",
			localPath, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DownloadFile copy file từ remote về local qua scp (hỗ trợ ~/.ssh/config và ProxyJump).
func (c *SSHClient) DownloadFile(ctx context.Context, remotePath, localPath string) error {
	_ = os.MkdirAll(filepath.Dir(localPath), 0755)
	args := c.scpBaseArgs()
	src := fmt.Sprintf("%s:%s", c.scpHostPrefix(), remotePath)
	args = append(args, src, localPath)

	execCmd := exec.CommandContext(ctx, "scp", args...)
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp download [%s -> %s] thất bại: %w (%s)",
			src, localPath, err, strings.TrimSpace(string(out)))
	}
	return nil
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
