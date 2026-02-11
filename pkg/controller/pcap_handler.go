package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
)

type CrictlInspectResult struct {
	Info struct {
		Pid int `json:"pid"`
	} `json:"info"`
}

func (c *Controller) startCapture(pod *corev1.Pod, captureFiles string) (int, error) {
	fmt.Printf("Handler: Starting capture for Pod '%s' (Max files: %s)\n", pod.Name, captureFiles)

	if pod.Status.PodIP == "" {
		return 0, fmt.Errorf("pod %s has no IP assigned yet", pod.Name)
	}

	// verifies the pod has running containers
	if len(pod.Status.ContainerStatuses) == 0 {
		return 0, fmt.Errorf("pod has no container statuses")
	}

	containerIDFull := pod.Status.ContainerStatuses[0].ContainerID

	// sanitize the ID
	containerID := strings.TrimPrefix(containerIDFull, "containerd://")
	containerID = strings.TrimPrefix(containerID, "docker://")
	containerID = strings.TrimPrefix(containerID, "cri-o://")

	if containerID == "" {
		return 0, fmt.Errorf("could not parse container ID from status: %s", containerIDFull)
	}

	// used crictl to ask the container runtime for the Host PID of this container.
	targetPID, err := getPIDFromContainerID(containerID)
	if err != nil {
		return 0, fmt.Errorf("failed to find PID for container %s: %v", containerID, err)
	}

	outputFilePrefix := fmt.Sprintf("/captures/%s.pcap", pod.Name)

	cmd := exec.Command("nsenter",
		"-t", fmt.Sprintf("%d", targetPID),
		"-n",
		"tcpdump",
		"-i", "any",
		"-U",
		"-C", "1",
		"-W", captureFiles,
		"-w", outputFilePrefix,
		"-Z", "root",
		"host", pod.Status.PodIP,
	)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start nsenter/tcpdump: %v", err)
	}

	return cmd.Process.Pid, nil
}

func (c *Controller) stopCapture(pid int, podName string) error {
	fmt.Printf("Handler: Stopping capture for Pod '%s' (PID: %d)\n", podName, pid)

	process, err := os.FindProcess(pid)
	if err == nil {
		// Send SIGTERM to allow tcpdump to close the file handle gracefully.
		_ = process.Signal(syscall.SIGTERM)
	} else {
		fmt.Printf("Warning: Process %d not found, skipping kill signal.\n", pid)
	}

	// delete pcap files based using the naming convention

	pattern := fmt.Sprintf("/captures/%s.pcap*", podName)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to list pcap files for cleanup: %v", err)
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil {
			fmt.Printf("Warning: Failed to delete file %s: %v\n", f, err)
		} else {
			fmt.Printf("Deleted file: %s\n", f)
		}
	}

	return nil
}

func getPIDFromContainerID(containerID string) (int, error) {

	cmd := exec.Command("crictl", "inspect", "--output", "json", containerID)

	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("crictl execution failed: %v", err)
	}

	var result CrictlInspectResult
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, fmt.Errorf("failed to parse crictl output: %v", err)
	}

	if result.Info.Pid == 0 {
		return 0, fmt.Errorf("crictl returned invalid PID (0)")
	}

	return result.Info.Pid, nil
}
