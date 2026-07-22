package module

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func GetAgentsNamespace() string {
	if ns := os.Getenv("AGENTS_NAMESPACE"); ns != "" {
		return ns
	}
	return "axiom-agents"
}

// GetCodeExecutorNamespace is deprecated, use GetAgentsNamespace
func GetCodeExecutorNamespace() string {
	return GetAgentsNamespace()
}

func GetCodeExecutorImage() string {
	if img := os.Getenv("CODE_EXECUTOR_IMAGE"); img != "" {
		return img
	}
	return "python:3.11-slim"
}

// GetBrewProcessRunnerImage returns the host image used for explicitly
// governed process-backed Skill actions. An empty value keeps the adapter
// unavailable instead of inventing a runtime.
func GetBrewProcessRunnerImage() string {
	return strings.TrimSpace(os.Getenv("OPENSEAL_BREW_RUNNER_IMAGE"))
}

// ProcessExecutorEnabled lets embedding hosts remove the procedural adapter
// entirely. It defaults on only for compatibility with an explicitly
// configured runner image; invalid values fail closed.
func ProcessExecutorEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("OPENSEAL_PROCESS_EXECUTOR_ENABLED"))
	if raw == "" {
		return GetBrewProcessRunnerImage() != ""
	}
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled && GetBrewProcessRunnerImage() != ""
}

func NewRegistryConfigFromEnv() (*RegistryConfig, error) {
	return &RegistryConfig{
		Namespace:   GetCodeExecutorNamespace(),
		PythonImage: GetCodeExecutorImage(),
	}, nil
}

// GetK8sClient returns a k8s client, using local kubeconfig if RUNTIME_CONFIG_LOCAL_DEV=true
func GetK8sClient() (kubernetes.Interface, error) {
	localDevMode := strings.ToLower(os.Getenv("RUNTIME_CONFIG_LOCAL_DEV")) == "true"

	var config *rest.Config
	var err error

	if localDevMode {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}

	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

// GetJobTTLSecondsAfterFinished returns how long completed/failed Jobs are kept for debugging.
// Default is 3600 seconds (1 hour). Configure via AGENT_JOB_TTL_SECONDS environment variable.
func GetJobTTLSecondsAfterFinished() int32 {
	if ttl := os.Getenv("AGENT_JOB_TTL_SECONDS"); ttl != "" {
		if val, err := strconv.ParseInt(ttl, 10, 32); err == nil && val > 0 {
			return int32(val)
		}
	}
	return 3600 // 1 hour default
}
