package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// K8sClient điều phối cụm Kubernetes qua SSH (Machine 3) hoặc kubectl cục bộ.
type K8sClient struct {
	sshClient      *SSHClient
	useSudo        bool
	namespace      string
	kubeconfigPath string
	contextName    string
}

// NewK8sClient khởi tạo client Kubernetes.
func NewK8sClient(sshClient *SSHClient, useSudo bool, namespace, kubeconfig, contextName string) *K8sClient {
	if namespace == "" {
		namespace = "default"
	}
	return &K8sClient{
		sshClient:      sshClient,
		useSudo:        useSudo,
		namespace:      namespace,
		kubeconfigPath: kubeconfig,
		contextName:    contextName,
	}
}

// RunHostCommand thực thi lệnh shell trên máy chạy kubectl (Machine 3 qua SSH hoặc Local).
func (k *K8sClient) RunHostCommand(ctx context.Context, cmd string) (string, error) {
	if k.sshClient != nil {
		return k.sshClient.RunCommand(ctx, cmd)
	}
	execCmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("lệnh host thất bại: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RunKubectl thực thi một lệnh kubectl (cục bộ hoặc qua SSH).
func (k *K8sClient) RunKubectl(ctx context.Context, args ...string) (string, error) {
	cmdArr := []string{}
	if k.useSudo {
		cmdArr = append(cmdArr, "sudo")
	}
	cmdArr = append(cmdArr, "kubectl")
	if k.kubeconfigPath != "" {
		cmdArr = append(cmdArr, fmt.Sprintf("--kubeconfig=%s", k.kubeconfigPath))
	}
	if k.contextName != "" {
		cmdArr = append(cmdArr, fmt.Sprintf("--context=%s", k.contextName))
	}
	cmdArr = append(cmdArr, args...)

	if k.sshClient != nil {
		// Escape từng argument để an toàn trên remote shell
		quoted := make([]string, len(cmdArr))
		for i, a := range cmdArr {
			quoted[i] = quoteShellArg(a)
		}
		fullCmd := strings.Join(quoted, " ")
		return k.sshClient.RunCommand(ctx, fullCmd)
	}

	// Thực thi trực tiếp trên máy cục bộ
	execCmd := exec.CommandContext(ctx, cmdArr[0], cmdArr[1:]...)
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("kubectl thất bại: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ClusterInfo kiểm tra kết nối API Server.
func (k *K8sClient) ClusterInfo(ctx context.Context) (string, error) {
	return k.RunKubectl(ctx, "cluster-info")
}

// ResolveTargetPod tự động tìm Pod đang chạy theo nhãn selector.
func (k *K8sClient) ResolveTargetPod(ctx context.Context, selector, explicitPod string) (string, error) {
	if explicitPod != "" {
		return explicitPod, nil
	}
	if selector == "" {
		return "", fmt.Errorf("cần chỉ định pod hoặc selector")
	}

	out, err := k.RunKubectl(ctx, "get", "pods", "-n", k.namespace, "-l", selector,
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}")
	if err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out), nil
	}

	// Fallback nếu không có phase=Running
	out, err = k.RunKubectl(ctx, "get", "pods", "-n", k.namespace, "-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("không tìm thấy Pod nào khớp với selector '%s' trong namespace '%s'", selector, k.namespace)
	}
	return strings.TrimSpace(out), nil
}

// DetectTargetWorkload tự động suy ra Deployment / StatefulSet quản lý Pod qua ownerReferences.
func (k *K8sClient) DetectTargetWorkload(ctx context.Context, podName, selector string) (workloadType, workloadName string, err error) {
	// 1. Kiểm tra ownerReferences của Pod
	ownerKind, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}")
	ownerName, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].name}")

	if ownerKind == "" {
		ownerKind, _ = k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			"-o", "jsonpath={.metadata.ownerReferences[0].kind}")
		ownerName, _ = k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			"-o", "jsonpath={.metadata.ownerReferences[0].name}")
	}

	ownerKind = strings.TrimSpace(ownerKind)
	ownerName = strings.TrimSpace(ownerName)

	if ownerKind == "ReplicaSet" {
		rsOwnerKind, _ := k.RunKubectl(ctx, "get", "rs", ownerName, "-n", k.namespace,
			"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}")
		rsOwnerName, _ := k.RunKubectl(ctx, "get", "rs", ownerName, "-n", k.namespace,
			"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].name}")
		if rsOwnerKind == "" {
			rsOwnerKind, _ = k.RunKubectl(ctx, "get", "rs", ownerName, "-n", k.namespace,
				"-o", "jsonpath={.metadata.ownerReferences[0].kind}")
			rsOwnerName, _ = k.RunKubectl(ctx, "get", "rs", ownerName, "-n", k.namespace,
				"-o", "jsonpath={.metadata.ownerReferences[0].name}")
		}
		if strings.TrimSpace(rsOwnerKind) == "Deployment" {
			return "deployment", strings.TrimSpace(rsOwnerName), nil
		}
	} else if ownerKind == "Deployment" {
		return "deployment", ownerName, nil
	} else if ownerKind == "StatefulSet" {
		return "statefulset", ownerName, nil
	}

	// 2. Fallback: tìm Deployment qua selector
	if selector != "" {
		depOut, err := k.RunKubectl(ctx, "get", "deployment", "-n", k.namespace, "-l", selector,
			"-o", "jsonpath={.items[0].metadata.name}")
		if err == nil && strings.TrimSpace(depOut) != "" {
			return "deployment", strings.TrimSpace(depOut), nil
		}
	}

	return "", "", fmt.Errorf("không thể nhận diện Deployment/StatefulSet quản lý Pod '%s'", podName)
}

// GetContainerName lấy tên container đầu tiên nếu chưa có.
func (k *K8sClient) GetContainerName(ctx context.Context, podName, containerName string) (string, error) {
	if containerName != "" {
		return containerName, nil
	}
	out, err := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.spec.containers[0].name}")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("không thể lấy containerName cho Pod %s: %w", podName, err)
	}
	return strings.TrimSpace(out), nil
}

// ApplyWorkloadCPU lưu cấu hình cũ, set CPU về mức chỉ định và chờ rollout hoàn tất.
func (k *K8sClient) ApplyWorkloadCPU(ctx context.Context, workloadType, workloadName, containerName, cpu string) (origResourcesJSON string, containerIdx int, err error) {
	// 1. Tìm container index
	outList, err := k.RunKubectl(ctx, "get", workloadType, workloadName, "-n", k.namespace,
		"-o", "jsonpath={range .spec.template.spec.containers[*]}{.name}{\"\\n\"}{end}")
	if err != nil {
		return "", 0, fmt.Errorf("không đọc được danh sách containers: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(outList), "\n")
	containerIdx = 0
	for i, l := range lines {
		if strings.TrimSpace(l) == containerName {
			containerIdx = i
			break
		}
	}

	// 2. Lưu snapshot resources ban đầu
	resOut, err := k.RunKubectl(ctx, "get", workloadType, workloadName, "-n", k.namespace,
		"-o", fmt.Sprintf("jsonpath={.spec.template.spec.containers[%d].resources}", containerIdx))
	if err == nil && strings.TrimSpace(resOut) != "" {
		origResourcesJSON = strings.TrimSpace(resOut)
	} else {
		origResourcesJSON = "{}"
	}

	// 3. Set resources đồng thời limits và requests để tránh lỗi validation
	targetRes := fmt.Sprintf("%s/%s", workloadType, workloadName)
	setArgs := []string{"set", "resources", targetRes, "-n", k.namespace}
	if containerName != "" {
		setArgs = append(setArgs, "-c", containerName)
	}
	setArgs = append(setArgs, fmt.Sprintf("--limits=cpu=%s", cpu), fmt.Sprintf("--requests=cpu=%s", cpu))

	if _, err := k.RunKubectl(ctx, setArgs...); err != nil {
		return "", 0, fmt.Errorf("lỗi khi set resources: %w", err)
	}

	// 4. Chờ rollout hoàn tất
	if _, err := k.RunKubectl(ctx, "rollout", "status", targetRes, "-n", k.namespace, "--timeout=180s"); err != nil {
		return origResourcesJSON, containerIdx, fmt.Errorf("lỗi khi chờ rollout: %w", err)
	}

	return origResourcesJSON, containerIdx, nil
}

// RestoreWorkloadCPU khôi phục lại tài nguyên ban đầu bằng JSON patch nguyên tử.
func (k *K8sClient) RestoreWorkloadCPU(ctx context.Context, workloadType, workloadName string, containerIdx int, origResourcesJSON string) error {
	if origResourcesJSON == "" {
		origResourcesJSON = "{}"
	}

	patchPayload := fmt.Sprintf(`[{"op": "replace", "path": "/spec/template/spec/containers/%d/resources", "value": %s}]`,
		containerIdx, origResourcesJSON)

	_, err := k.RunKubectl(ctx, "patch", workloadType, workloadName, "-n", k.namespace,
		"--type=json", "-p", patchPayload)
	if err != nil {
		return fmt.Errorf("lỗi patch khôi phục tài nguyên: %w", err)
	}

	targetRes := fmt.Sprintf("%s/%s", workloadType, workloadName)
	_, _ = k.RunKubectl(ctx, "rollout", "status", targetRes, "-n", k.namespace, "--timeout=180s")
	return nil
}

// WaitForPodReady chờ Pod mới xuất hiện và sẵn sàng sau rollout.
func (k *K8sClient) WaitForPodReady(ctx context.Context, selector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		pod, err := k.ResolveTargetPod(ctx, selector, "")
		if err == nil && pod != "" {
			ready, _ := k.RunKubectl(ctx, "get", "pod", pod, "-n", k.namespace,
				"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
			if strings.EqualFold(strings.TrimSpace(ready), "true") {
				return pod, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("hết thời gian chờ Pod sẵn sàng khớp với selector %s", selector)
}

// SetupDebugContainer gắn Ephemeral Debug Container (capsizer-debugger) vào Pod mục tiêu.
// Trả về tên Pod thực tế (có thể đổi sang Pod mới nếu Pod cũ có debug container bị terminated).
func (k *K8sClient) SetupDebugContainer(ctx context.Context, podName, containerName, sidecarImage, selector string) (string, error) {
	debugContainerName := "capsizer-debugger"
	if sidecarImage == "" {
		sidecarImage = "docker.io/duyle95/profiling-sidecar:latest"
	}

	targetContainer, err := k.GetContainerName(ctx, podName, containerName)
	if err != nil {
		targetContainer = containerName
	}

	// Kiểm tra xem debug container đã tồn tại chưa
	hasDebug, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.spec.ephemeralContainers[*].name}")
	if strings.Contains(hasDebug, debugContainerName) {
		dbgState, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			fmt.Sprintf("-o=jsonpath={.status.ephemeralContainerStatuses[?(@.name=='%s')].state}", debugContainerName))
		if strings.Contains(dbgState, "running") {
			fmt.Printf("    ✓ Ephemeral container '%s' đã chạy sẵn trong Pod %s.\n", debugContainerName, podName)
			return podName, nil
		}
		if strings.Contains(dbgState, "terminated") && selector != "" {
			fmt.Printf("    [!] Ephemeral container '%s' cũ đã dừng (terminated). Đang tái tạo Pod sạch...\n", debugContainerName)
			_, _ = k.RunKubectl(ctx, "delete", "pod", podName, "-n", k.namespace, "--wait=true")
			newPod, err := k.WaitForPodReady(ctx, selector, 120*time.Second)
			if err != nil {
				return podName, fmt.Errorf("không thể chờ Pod mới sau khi xóa Pod cũ: %w", err)
			}
			podName = newPod
			fmt.Printf("    ✓ Pod mới đã sẵn sàng: %s\n", podName)
		}
	}

	// Tạo file cấu hình securityContext root tạm trên máy chạy kubectl
	customYAMLPath := "/tmp/capsizer-debug-root.yaml"
	_, _ = k.RunHostCommand(ctx, fmt.Sprintf("printf 'securityContext:\\n  runAsUser: 0\\n  runAsGroup: 0\\n' > %s", customYAMLPath))

	debugArgs := []string{
		"debug", podName, "-n", k.namespace,
		fmt.Sprintf("--image=%s", sidecarImage),
	}
	if targetContainer != "" {
		debugArgs = append(debugArgs, fmt.Sprintf("--target=%s", targetContainer))
	}
	debugArgsWithCustom := append([]string{}, debugArgs...)
	debugArgsWithCustom = append(debugArgsWithCustom,
		"-c", debugContainerName,
		"--attach=false",
		fmt.Sprintf("--custom=%s", customYAMLPath),
		"--profile=sysadmin",
		"--", "sh", "-c", "sleep infinity",
	)

	if _, err := k.RunKubectl(ctx, debugArgsWithCustom...); err != nil {
		// Fallback nếu phiên bản kubectl không hỗ trợ --custom hoặc --profile
		debugArgsFallback := append([]string{}, debugArgs...)
		debugArgsFallback = append(debugArgsFallback,
			"-c", debugContainerName,
			"--attach=false",
			"--", "sh", "-c", "sleep infinity",
		)
		if _, err2 := k.RunKubectl(ctx, debugArgsFallback...); err2 != nil {
			return podName, fmt.Errorf("lỗi gắn ephemeral debug container: %w", err2)
		}
	}

	// Đợi container running
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		dbgState, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			fmt.Sprintf("-o=jsonpath={.status.ephemeralContainerStatuses[?(@.name=='%s')].state}", debugContainerName))
		if strings.Contains(dbgState, "running") {
			fmt.Printf("    ✓ Ephemeral container '%s' đã sẵn sàng (running)!\n", debugContainerName)
			return podName, nil
		}
	}

	return podName, fmt.Errorf("ephemeral container %s không đạt trạng thái running", debugContainerName)
}

// ExecInPod thực thi lệnh shell bên trong container của Pod.
func (k *K8sClient) ExecInPod(ctx context.Context, podName, containerName, cmd string) (string, error) {
	args := []string{"exec", "-n", k.namespace, podName}
	if containerName != "" {
		args = append(args, "-c", containerName)
	}
	args = append(args, "--", "sh", "-c", cmd)
	return k.RunKubectl(ctx, args...)
}

// CopyFromPod đọc nội dung file từ bên trong Pod và ghi trực tiếp về máy local
// qua luồng stdout của kubectl exec (không tạo file tạm và không dùng lệnh xóa trên Server K8s).
func (k *K8sClient) CopyFromPod(ctx context.Context, podName, containerName, remotePath, localPath string) error {
	content, err := k.ExecInPod(ctx, podName, containerName, fmt.Sprintf("cat %s", quoteShellArg(remotePath)))
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("file %s trong Pod %s rỗng hoặc không tồn tại", remotePath, podName)
	}
	_ = os.MkdirAll(filepath.Dir(localPath), 0755)
	return os.WriteFile(localPath, []byte(content+"\n"), 0644)
}

// CopyToPod sao chép file từ máy local vào bên trong Pod.
func (k *K8sClient) CopyToPod(ctx context.Context, localPath, podName, containerName, remotePath string) error {
	dst := fmt.Sprintf("%s/%s:%s", k.namespace, podName, remotePath)

	if k.sshClient != nil {
		srvStaging := "/tmp/capsizer-staging-bin"
		if err := k.sshClient.UploadFile(ctx, localPath, srvStaging); err != nil {
			return err
		}
		args := []string{"cp", srvStaging, dst}
		if containerName != "" {
			args = append(args, "-c", containerName)
		}
		_, err := k.RunKubectl(ctx, args...)
		return err
	}

	args := []string{"cp", localPath, dst}
	if containerName != "" {
		args = append(args, "-c", containerName)
	}
	_, err := k.RunKubectl(ctx, args...)
	return err
}

func quoteShellArg(arg string) string {
	if strings.ContainsAny(arg, " \t\n\"'$()[]{}*?&;|<>\\`~#") {
		return strconv.Quote(arg)
	}
	return arg
}
