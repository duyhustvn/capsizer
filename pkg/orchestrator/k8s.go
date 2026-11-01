package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// K8sClient điều phối cụm Kubernetes qua SSH (Machine 3) hoặc kubectl cục bộ.
type K8sClient struct {
	sshClient       *SSHClient
	useSudo         bool
	namespace       string
	kubeconfigPath  string
	contextName     string
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

// SetupDebugContainer gắn Ephemeral Debug Container vào Pod mục tiêu.
func (k *K8sClient) SetupDebugContainer(ctx context.Context, podName, containerName, sidecarImage string) error {
	debugContainerName := "capsizer-debugger"
	if sidecarImage == "" {
		sidecarImage = "docker.io/duyle95/profiling-sidecar:latest"
	}

	// Kiểm tra xem debug container đã tồn tại chưa
	hasDebug, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.spec.ephemeralContainers[*].name}")
	if strings.Contains(hasDebug, debugContainerName) {
		dbgState, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			fmt.Sprintf("-o=jsonpath={.status.ephemeralContainerStatuses[?(@.name=='%s')].state}", debugContainerName))
		if strings.Contains(dbgState, "running") {
			return nil
		}
	}

	debugArgs := []string{
		"debug", podName, "-n", k.namespace,
		fmt.Sprintf("--image=%s", sidecarImage),
		fmt.Sprintf("--custom-name=%s", debugContainerName),
		"--share-processes",
	}
	if containerName != "" {
		debugArgs = append(debugArgs, fmt.Sprintf("--target=%s", containerName))
	}

	_, err := k.RunKubectl(ctx, debugArgs...)
	if err != nil {
		return fmt.Errorf("lỗi gắn ephemeral debug container: %w", err)
	}

	// Đợi container running
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		dbgState, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			fmt.Sprintf("-o=jsonpath={.status.ephemeralContainerStatuses[?(@.name=='%s')].state}", debugContainerName))
		if strings.Contains(dbgState, "running") {
			return nil
		}
	}

	return fmt.Errorf("ephemeral container %s không đạt trạng thái running", debugContainerName)
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

// CopyFromPod sao chép file từ Pod về máy local.
func (k *K8sClient) CopyFromPod(ctx context.Context, podName, containerName, remotePath, localPath string) error {
	args := []string{"cp"}
	src := fmt.Sprintf("%s/%s:%s", k.namespace, podName, remotePath)
	args = append(args, src)

	if k.sshClient != nil {
		// Tạo file tạm trên server K8s trước, sau đó SCP về máy Skill
		srvTmp := fmt.Sprintf("/tmp/_capsizer_cp_%d.tmp", time.Now().UnixNano())
		args = append(args, srvTmp)
		if containerName != "" {
			args = append(args, "-c", containerName)
		}
		if _, err := k.RunKubectl(ctx, args...); err != nil {
			return err
		}
		defer func() {
			_, _ = k.RunKubectl(ctx, "rm", "-f", srvTmp)
		}()
		return k.sshClient.DownloadFile(ctx, srvTmp, localPath)
	}

	// Trực tiếp trên máy local
	args = append(args, localPath)
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
