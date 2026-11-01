package orchestrator

import (
	"bufio"
	"bytes"
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
// Trả về tên Pod thực tế (có thể đổi sang Pod mới nếu Pod cũ có debug container bị lỗi hoặc terminated).
func (k *K8sClient) SetupDebugContainer(ctx context.Context, podName, containerName, sidecarImage, selector string) (string, error) {
	debugContainerName := "capsizer-debugger"
	if sidecarImage == "" {
		sidecarImage = "docker.io/duyle95/profiling-sidecar:latest"
	}

	targetContainer, err := k.GetContainerName(ctx, podName, containerName)
	if err != nil {
		targetContainer = containerName
	}

	// Kiểm tra chính sách PSA của Namespace và securityContext của Pod
	psaEnforce, _ := k.RunKubectl(ctx, "get", "ns", k.namespace,
		"-o", "jsonpath={.metadata.labels.pod-security\\.kubernetes\\.io/enforce}")
	podSecCtx, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.spec.securityContext}")
	ctrSecCtx, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
		"-o", "jsonpath={.spec.containers[0].securityContext}")
	if strings.TrimSpace(psaEnforce) != "" || (strings.TrimSpace(podSecCtx) != "" && strings.TrimSpace(podSecCtx) != "{}") {
		fmt.Printf("    • Chính sách bảo mật: Namespace PSA enforce='%s' | Pod securityContext=%s | Container securityContext=%s\n",
			strings.TrimSpace(psaEnforce), strings.TrimSpace(podSecCtx), strings.TrimSpace(ctrSecCtx))
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
		// Nếu container debug cũ bị terminated HOẶC kẹt ở CreateContainerConfigError / CreateContainerError
		if selector != "" {
			fmt.Printf("    [!] Ephemeral container '%s' cũ trên Pod %s không khả dụng (%s). Đang tái tạo Pod sạch...\n",
				debugContainerName, podName, strings.TrimSpace(dbgState))
			oldPod := podName
			_, _ = k.RunKubectl(ctx, "delete", "pod", oldPod, "-n", k.namespace, "--wait=false")
			newPod, err := k.WaitForNewPodReady(ctx, selector, oldPod, 120*time.Second)
			if err != nil {
				return podName, fmt.Errorf("không thể chờ Pod mới sau khi xóa Pod cũ %s: %w", oldPod, err)
			}
			podName = newPod
			fmt.Printf("    ✓ Pod mới đã sẵn sàng: %s\n", podName)
		} else {
			return podName, fmt.Errorf("ephemeral container '%s' trên Pod %s đang lỗi (%s); hãy truyền --selector hoặc xóa Pod để tạo lại",
				debugContainerName, podName, strings.TrimSpace(dbgState))
		}
	}

	if err := k.attachDebugContainerWithName(ctx, podName, targetContainer, sidecarImage, debugContainerName); err != nil {
		return podName, err
	}
	return podName, nil
}

// WaitForNewPodReady chờ một Pod mới (khác với oldPod) xuất hiện và đạt trạng thái Ready.
func (k *K8sClient) WaitForNewPodReady(ctx context.Context, selector, oldPod string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		out, err := k.RunKubectl(ctx, "get", "pods", "-n", k.namespace, "-l", selector,
			"--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}")
		if err == nil && strings.TrimSpace(out) != "" {
			for _, candidate := range strings.Fields(out) {
				if candidate != "" && candidate != oldPod {
					ready, _ := k.RunKubectl(ctx, "get", "pod", candidate, "-n", k.namespace,
						"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
					if strings.EqualFold(strings.TrimSpace(ready), "true") {
						return candidate, nil
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("hết thời gian chờ Pod mới sẵn sàng thay thế %s", oldPod)
}

// PodContainerStatus chứa trạng thái sức khỏe của Pod, container ứng dụng và debug container.
type PodContainerStatus struct {
	PodPhase         string
	PodReady         bool
	RestartCount     int
	LastTermReason   string
	LastTermExitCode int
	WaitingReason    string
	DebugRunning     bool
	DebugStateRaw    string
}

// GetPodContainerStatus truy vấn trạng thái hiện tại của Pod, số lần restart của app và trạng thái debug container.
func (k *K8sClient) GetPodContainerStatus(ctx context.Context, podName, appContainer, debugContainer string) (*PodContainerStatus, error) {
	st := &PodContainerStatus{}
	if podName == "" {
		return st, nil
	}

	// Truy vấn gộp các trường qua jsonpath phân tách bằng '|'
	jp := fmt.Sprintf(
		"{.status.phase}|"+
			"{.status.conditions[?(@.type==\"Ready\")].status}|"+
			"{.status.containerStatuses[0].restartCount}|"+
			"{.status.containerStatuses[0].lastState.terminated.reason}|"+
			"{.status.containerStatuses[0].lastState.terminated.exitCode}|"+
			"{.status.containerStatuses[0].state.waiting.reason}|"+
			"{.status.ephemeralContainerStatuses[?(@.name=='%s')].state}",
		debugContainer,
	)

	out, err := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace, "-o", "jsonpath="+jp)
	if err != nil {
		return st, err
	}

	parts := strings.SplitN(strings.TrimSpace(out), "|", 7)
	if len(parts) >= 1 {
		st.PodPhase = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 2 {
		st.PodReady = strings.EqualFold(strings.TrimSpace(parts[1]), "true")
	}
	if len(parts) >= 3 {
		st.RestartCount, _ = strconv.Atoi(strings.TrimSpace(parts[2]))
	}
	if len(parts) >= 4 {
		st.LastTermReason = strings.TrimSpace(parts[3])
	}
	if len(parts) >= 5 {
		st.LastTermExitCode, _ = strconv.Atoi(strings.TrimSpace(parts[4]))
	}
	if len(parts) >= 6 {
		st.WaitingReason = strings.TrimSpace(parts[5])
	}
	if len(parts) >= 7 {
		st.DebugStateRaw = strings.TrimSpace(parts[6])
		st.DebugRunning = strings.Contains(st.DebugStateRaw, "running")
	}

	return st, nil
}

// attachDebugContainerWithName gắn một ephemeral debug container với tên chỉ định vào Pod.
func (k *K8sClient) attachDebugContainerWithName(ctx context.Context, podName, targetContainer, sidecarImage, debugContainerName string) error {
	fullRootYAML := "/tmp/capsizer-debug-root.yaml"
	minRootYAML := "/tmp/capsizer-debug-root-min.yaml"
	baselineRootYAML := "/tmp/capsizer-debug-root-baseline.yaml"

	prepCmd := fmt.Sprintf(
		"cat << 'EOF' > %s\n"+
			"securityContext:\n"+
			"  runAsUser: 0\n"+
			"  runAsGroup: 0\n"+
			"  runAsNonRoot: false\n"+
			"  allowPrivilegeEscalation: true\n"+
			"  capabilities:\n"+
			"    add:\n"+
			"      - SYS_PTRACE\n"+
			"      - SYS_ADMIN\n"+
			"EOF\n"+
			"cat << 'EOF' > %s\n"+
			"securityContext:\n"+
			"  runAsUser: 0\n"+
			"  runAsGroup: 0\n"+
			"  runAsNonRoot: false\n"+
			"  allowPrivilegeEscalation: true\n"+
			"  capabilities:\n"+
			"    add:\n"+
			"      - SYS_PTRACE\n"+
			"EOF\n"+
			"cat << 'EOF' > %s\n"+
			"securityContext:\n"+
			"  runAsUser: 0\n"+
			"  runAsGroup: 0\n"+
			"  runAsNonRoot: false\n"+
			"EOF",
		fullRootYAML, minRootYAML, baselineRootYAML,
	)
	_, _ = k.RunHostCommand(ctx, prepCmd)

	baseArgs := []string{
		"debug", podName, "-n", k.namespace,
		"--attach=false",
		"-c", debugContainerName,
		fmt.Sprintf("--image=%s", sidecarImage),
	}
	if targetContainer != "" {
		baseArgs = append(baseArgs, fmt.Sprintf("--target=%s", targetContainer))
	}

	attempts := [][]string{
		append(append([]string{}, baseArgs...), fmt.Sprintf("--custom=%s", fullRootYAML), "--profile=sysadmin", "--", "sh", "-c", "sleep infinity"),
		append(append([]string{}, baseArgs...), fmt.Sprintf("--custom=%s", minRootYAML), "--", "sh", "-c", "sleep infinity"),
		append(append([]string{}, baseArgs...), fmt.Sprintf("--custom=%s", baselineRootYAML), "--", "sh", "-c", "sleep infinity"),
	}

	var lastErr error
	attached := false
	for _, args := range attempts {
		if _, err := k.RunKubectl(ctx, args...); err == nil {
			attached = true
			break
		} else {
			lastErr = err
		}
	}

	if !attached {
		nonRootYAML := "/tmp/capsizer-debug-nonroot.yaml"
		nonRootCmd := fmt.Sprintf(
			"cat << 'EOF' > %s\n"+
				"securityContext:\n"+
				"  runAsUser: 1000\n"+
				"  runAsGroup: 1000\n"+
				"  runAsNonRoot: true\n"+
				"  allowPrivilegeEscalation: false\n"+
				"  capabilities:\n"+
				"    drop:\n"+
				"      - ALL\n"+
				"  seccompProfile:\n"+
				"    type: RuntimeDefault\n"+
				"EOF",
			nonRootYAML,
		)
		_, _ = k.RunHostCommand(ctx, nonRootCmd)
		nonRootArgs := append(append([]string{}, baseArgs...), fmt.Sprintf("--custom=%s", nonRootYAML), "--", "sh", "-c", "sleep infinity")
		if _, err := k.RunKubectl(ctx, nonRootArgs...); err == nil {
			fmt.Println("    [CẢNH BÁO] Namespace chặn quyền root (PSA restricted); đã gắn debug container ở chế độ non-root (UID 1000).")
			attached = true
		} else {
			return fmt.Errorf("lỗi gắn ephemeral debug container %s: %v (lỗi trước đó: %v)", debugContainerName, err, lastErr)
		}
	}

	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		dbgState, _ := k.RunKubectl(ctx, "get", "pod", podName, "-n", k.namespace,
			fmt.Sprintf("-o=jsonpath={.status.ephemeralContainerStatuses[?(@.name=='%s')].state}", debugContainerName))
		if strings.Contains(dbgState, "running") {
			fmt.Printf("    ✓ Ephemeral container '%s' đã sẵn sàng (running)!\n", debugContainerName)
			return nil
		}
		if strings.Contains(dbgState, "CreateContainerConfigError") || strings.Contains(dbgState, "CreateContainerError") {
			return fmt.Errorf("ephemeral container '%s' bị Kubelet từ chối khởi tạo: %s", debugContainerName, strings.TrimSpace(dbgState))
		}
	}

	return fmt.Errorf("ephemeral container %s không đạt trạng thái running", debugContainerName)
}

// EnsureDebugContainerAlive kiểm tra Pod và debug container trước Pha 2;
// nếu app container bị restart ở Pha 1 làm chết debug container cũ, tự động chờ Pod Ready và gắn lại debug container mới.
func (k *K8sClient) EnsureDebugContainerAlive(ctx context.Context, podName, containerName, sidecarImage, selector, currentDebugContainer string) (string, string, error) {
	if currentDebugContainer == "" {
		currentDebugContainer = "capsizer-debugger"
	}
	if sidecarImage == "" {
		sidecarImage = "docker.io/duyle95/profiling-sidecar:latest"
	}

	st, err := k.GetPodContainerStatus(ctx, podName, containerName, currentDebugContainer)
	if err == nil && st.PodReady && st.DebugRunning {
		return podName, currentDebugContainer, nil
	}

	if err == nil {
		fmt.Printf("    [Guard Pha 2] Trạng thái hiện tại của Pod %s: Ready=%v, RestartCount=%d, LastTerm=%s(exit=%d), Waiting=%s, DebugRunning=%v\n",
			podName, st.PodReady, st.RestartCount, st.LastTermReason, st.LastTermExitCode, st.WaitingReason, st.DebugRunning)
	}

	// 1. Nếu Pod đang restart hoặc chưa Ready, chờ tối đa 60s để Pod hồi phục (hoặc phát hiện CrashLoopBackOff)
	if st == nil || !st.PodReady {
		fmt.Printf("    • Đang chờ Pod %s hồi phục trạng thái Ready (tối đa 60s)...\n", podName)
		recovered := false
		for i := 0; i < 30; i++ {
			time.Sleep(2 * time.Second)
			curSt, err := k.GetPodContainerStatus(ctx, podName, containerName, currentDebugContainer)
			if err == nil && curSt.PodReady {
				st = curSt
				recovered = true
				fmt.Printf("    ✓ Pod %s đã hồi phục Ready!\n", podName)
				break
			}
			if err == nil && curSt.WaitingReason == "CrashLoopBackOff" && i >= 10 {
				return podName, currentDebugContainer, fmt.Errorf(
					"Pod %s đang rơi vào CrashLoopBackOff (restartCount=%d, lastReason=%s, exitCode=%d) — dừng Pha 2",
					podName, curSt.RestartCount, curSt.LastTermReason, curSt.LastTermExitCode)
			}
		}
		if !recovered {
			if selector != "" {
				if newPod, err := k.WaitForPodReady(ctx, selector, 30*time.Second); err == nil && newPod != "" {
					podName = newPod
				} else {
					return podName, currentDebugContainer, fmt.Errorf("Pod %s không hồi phục Ready sau sự cố ở Pha 1", podName)
				}
			} else {
				return podName, currentDebugContainer, fmt.Errorf("Pod %s không hồi phục Ready sau sự cố ở Pha 1", podName)
			}
		}
	}

	// Kiểm tra lại xem trên Pod hiện tại debug container có đang chạy không
	st, _ = k.GetPodContainerStatus(ctx, podName, containerName, currentDebugContainer)
	if st != nil && st.DebugRunning {
		return podName, currentDebugContainer, nil
	}

	// 2. Nếu debug container cũ đã chết (terminated do app container bị Liveness/OOM kill ở Pha 1),
	// gắn một ephemeral debug container mới với tên mới nhắm vào PID namespace mới của app container!
	targetContainer, _ := k.GetContainerName(ctx, podName, containerName)
	newDebugName := fmt.Sprintf("capsizer-debugger-%d", time.Now().Unix()%10000)
	fmt.Printf("    • Đang gắn Ephemeral Debug Container mới '%s' vào PID namespace mới của Pod %s...\n", newDebugName, podName)
	if err := k.attachDebugContainerWithName(ctx, podName, targetContainer, sidecarImage, newDebugName); err == nil {
		return podName, newDebugName, nil
	} else {
		fmt.Printf("    [!] Không thể gắn thêm '%s' trực tiếp (%v). Chuyển sang tái tạo Pod sạch...\n", newDebugName, err)
	}

	// Fallback: tái tạo Pod mới hoàn toàn
	newPod, err := k.SetupDebugContainer(ctx, podName, containerName, sidecarImage, selector)
	return newPod, "capsizer-debugger", err
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

// StreamExecInPod thực thi lệnh bên trong container của Pod và đọc luồng stdout theo thời gian thực (từng dòng).
func (k *K8sClient) StreamExecInPod(ctx context.Context, podName, containerName, cmd string, onLine func(string)) error {
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
	cmdArr = append(cmdArr, "exec", "-n", k.namespace, podName)
	if containerName != "" {
		cmdArr = append(cmdArr, "-c", containerName)
	}
	cmdArr = append(cmdArr, "--", "sh", "-c", cmd)

	if k.sshClient != nil {
		quoted := make([]string, len(cmdArr))
		for i, a := range cmdArr {
			quoted[i] = quoteShellArg(a)
		}
		fullCmd := strings.Join(quoted, " ")
		return k.sshClient.StreamCommand(ctx, fullCmd, onLine)
	}

	execCmd := exec.CommandContext(ctx, cmdArr[0], cmdArr[1:]...)
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
		return fmt.Errorf("kubectl exec stream thất bại: %w (%s)", waitErr, strings.TrimSpace(stderr.String()))
	}
	return scanner.Err()
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
