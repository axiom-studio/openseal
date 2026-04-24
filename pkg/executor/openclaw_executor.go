package executor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"

	"github.com/axiom-studio/openseal/pkg/module"
	"github.com/axiom-studio/openseal/pkg/skillmd"
)

const (
	NodeTypeOpenClaw       = "openclaw"
	openClawSkillDirEnvKey = "AXIOM_OPENCLAW_SKILLS_DIR"
	skillDefaultBaseImage  = "alpine:3.20"
)

type OpenClawExecutor struct {
	k8sClient  kubernetes.Interface
	namespace  string
	skillsDir  string
	ttlSeconds int32
}

func NewOpenClawExecutor(k8sClient kubernetes.Interface, namespace, skillsDir string) *OpenClawExecutor {
	return &OpenClawExecutor{
		k8sClient:  k8sClient,
		namespace:  namespace,
		skillsDir:  skillsDir,
		ttlSeconds: 300,
	}
}

func (e *OpenClawExecutor) Type() string {
	return NodeTypeOpenClaw
}

func (e *OpenClawExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("openclaw step requires config")
	}

	slug, _ := config["slug"].(string)
	if slug == "" {
		return nil, fmt.Errorf("openclaw step requires 'slug' in config")
	}

	command, _ := config["command"].(string)
	if command == "" {
		return nil, fmt.Errorf("openclaw step requires 'command'")
	}

	resolvedCommand := command
	if tr, ok := resolver.(interface{ ResolveString(string) string }); ok {
		resolvedCommand = tr.ResolveString(command)
	}

	content, err := os.ReadFile(fmt.Sprintf("%s/%s/SKILL.md", e.skillsDir, slug))
	if err != nil {
		return nil, fmt.Errorf("skill %q not found: %w", slug, err)
	}

	parsed, err := skillmd.ParseSkillMD(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SKILL.md for %q: %w", slug, err)
	}

	timeout := 5 * time.Minute
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	} else if t, ok := config["timeout"].(string); ok {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	cpuLimit := "500m"
	memLimit := "512Mi"
	if r, ok := config["resources"].(map[string]interface{}); ok {
		if c, ok := r["cpu"].(string); ok && c != "" {
			cpuLimit = c
		}
		if m, ok := r["memory"].(string); ok && m != "" {
			memLimit = m
		}
	}
	if c, ok := config["cpu"].(string); ok && c != "" {
		cpuLimit = c
	}
	if m, ok := config["memory"].(string); ok && m != "" {
		memLimit = m
	}

	image, initScript := buildInstallInit(parsed, resolvedCommand)

	envVars := buildEnvVars(parsed, config)

	jobName := fmt.Sprintf("oc-%s-%d", slug, time.Now().UnixNano())

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: e.namespace,
			Labels: map[string]string{
				"app":   "openclaw-skill",
				"skill": slug,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &e.ttlSeconds,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "skill",
							Image:   image,
							Command: []string{"sh", "-c"},
							Args:    []string{initScript},
							Env:     envVars,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cpuLimit),
									corev1.ResourceMemory: resource.MustParse(memLimit),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "skill", MountPath: "/skill", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "skill",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: fmt.Sprintf("%s/%s", e.skillsDir, slug),
									Type: func() *corev1.HostPathType { t := corev1.HostPathDirectory; return &t }(),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
			BackoffLimit: ptrInt32p(0),
		},
	}

	if _, err := e.k8sClient.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("failed to create openclaw job: %w", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-ctxWithTimeout.Done():
			e.k8sClient.BatchV1().Jobs(e.namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
			return nil, fmt.Errorf("openclaw skill %q timed out after %v", slug, timeout)
		default:
		}

		j, err := e.k8sClient.BatchV1().Jobs(e.namespace).Get(ctxWithTimeout, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get job status: %w", err)
		}

		if j.Status.Succeeded > 0 {
			logs, _ := e.readJobLogs(ctx, jobName, e.namespace)
			return &StepResult{
				Output: map[string]interface{}{
					"slug":    slug,
					"command": command,
					"stdout":  logs,
				},
			}, nil
		}

		if j.Status.Failed > 0 {
			logs, _ := e.readJobLogs(ctx, jobName, e.namespace)
			return nil, fmt.Errorf("openclaw skill %q failed:\n%s", slug, logs)
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func buildInstallInit(parsed *skillmd.ParsedSkill, command string) (string, string) {
	installSteps := parsed.Metadata.Install
	if len(installSteps) == 0 && len(parsed.Metadata.RequiresBins) == 0 {
		return "alpine:3.20", fmt.Sprintf("exec %s", shellQuote(command))
	}

	var apkPkgs []string
	var aptPkgs []string
	var preScripts []string
	needsDebian := false

	for _, spec := range installSteps {
		switch strings.ToLower(spec.Kind) {
		case "apk":
			apkPkgs = append(apkPkgs, spec.Package)
		case "apt":
			needsDebian = true
			aptPkgs = append(aptPkgs, spec.Package)
		case "brew":
		case "npm":
			pkg := spec.Package
			if pkg == "" {
				pkg = spec.Formula
			}
			if pkg != "" {
				preScripts = append(preScripts, fmt.Sprintf("npm install -g %s 2>/dev/null || true", pkg))
			}
		case "pip":
			pkg := spec.Package
			if pkg == "" {
				pkg = spec.Formula
			}
			if pkg != "" {
				preScripts = append(preScripts, fmt.Sprintf("pip install %s 2>/dev/null || true", pkg))
			}
		case "go":
			if spec.Package != "" {
				preScripts = append(preScripts, fmt.Sprintf("go install %s 2>/dev/null || true", spec.Package))
			}
		case "curl":
			if spec.Package != "" {
				preScripts = append(preScripts, fmt.Sprintf("curl -fsSL %s | sh 2>/dev/null || true", spec.Package))
			}
		case "script":
			if spec.Package != "" {
				preScripts = append(preScripts, spec.Package)
			}
		}
	}

	if !needsDebian && len(aptPkgs) == 0 {
		image := "alpine:3.20"
		var parts []string
		if len(apkPkgs) > 0 {
			parts = append(parts, fmt.Sprintf("apk add --no-cache %s", strings.Join(apkPkgs, " ")))
		}
		for _, s := range preScripts {
			parts = append(parts, s)
		}
		parts = append(parts, fmt.Sprintf("exec %s", shellQuote(command)))
		return image, strings.Join(parts, "\n")
	}

	image := "debian:bookworm-slim"
	var parts []string
	if len(aptPkgs) > 0 {
		parts = append(parts, "apt-get update && apt-get install -y "+strings.Join(aptPkgs, " "))
	}
	for _, s := range preScripts {
		parts = append(parts, s)
	}
	parts = append(parts, fmt.Sprintf("exec %s", shellQuote(command)))
	return image, strings.Join(parts, "\n")
}

func buildEnvVars(parsed *skillmd.ParsedSkill, config map[string]interface{}) []corev1.EnvVar {
	configEnv, _ := config["env"].(map[string]interface{})
	var env []corev1.EnvVar

	for _, name := range parsed.Metadata.RequiresEnv {
		val := ""
		if v, ok := configEnv[name]; ok {
			val = fmt.Sprintf("%v", v)
		}
		if v, ok := config[name].(string); ok && v != "" {
			val = v
		}
		if val != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: val})
		}
	}

	return env
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	s = strings.ReplaceAll(s, "'", "'\\''")
	return fmt.Sprintf("'%s'", s)
}

func (e *OpenClawExecutor) readJobLogs(ctx context.Context, jobName, namespace string) (string, error) {
	pods, err := e.k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}

	req := e.k8sClient.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var out strings.Builder
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		out.WriteString(scanner.Text() + "\n")
	}
	if out.Len() > 32*1024 {
		s := out.String()
		return s[len(s)-32*1024:], nil
	}
	return out.String(), nil
}

func (e *OpenClawSkillToolExecutor) readJobLogs(ctx context.Context, jobName, namespace string) (string, error) {
	pods, err := e.k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}

	req := e.k8sClient.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var out strings.Builder
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		out.WriteString(scanner.Text() + "\n")
	}
	if out.Len() > 32*1024 {
		s := out.String()
		return s[len(s)-32*1024:], nil
	}
	return out.String(), nil
}

func ptrInt32p(v int32) *int32 { return &v }

// OpenClawSkillToolExecutor wraps a skill tool definition and executes it in a container
type OpenClawSkillToolExecutor struct {
	def       *ToolDefinition
	resolver  TemplateResolver
	k8sClient kubernetes.Interface
	namespace string
	skillsDir string
	ttlSeconds int32
}

func NewOpenClawSkillToolExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	k8s, err := module.GetK8sClient()
	if err != nil {
		return nil, fmt.Errorf("openclaw skill tool requires kubernetes: %w", err)
	}
	skillsDir := os.Getenv(openClawSkillDirEnvKey)
	if skillsDir == "" {
		skillsDir = "/data/openclaw-skills"
	}
	return &OpenClawSkillToolExecutor{
		def:       def,
		resolver:  resolver,
		k8sClient: k8s,
		namespace: module.GetAgentsNamespace(),
		skillsDir: skillsDir,
		ttlSeconds: 300,
	}, nil
}

func (e *OpenClawSkillToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	skillName, _ := e.def.Config["skill_name"].(string)
	skillBody, _ := e.def.Config["body"].(string)

	commandTemplate, _ := args["command"].(string)
	if commandTemplate == "" {
		commandTemplate, _ = args["action"].(string)
	}
	if commandTemplate == "" {
		return nil, fmt.Errorf("openclaw skill %q requires 'command' argument", skillName)
	}

	command := commandTemplate
	if tr, ok := e.resolver.(interface{ ResolveString(string) string }); ok {
		command = tr.ResolveString(command)
	}

	content, err := os.ReadFile(fmt.Sprintf("%s/%s/SKILL.md", e.skillsDir, skillName))
	if err != nil {
		return nil, fmt.Errorf("skill %q not found: %w", skillName, err)
	}
	parsed, err := skillmd.ParseSkillMD(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SKILL.md for %q: %w", skillName, err)
	}

	timeout := 5 * time.Minute
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	cpuLimit := "500m"
	memLimit := "512Mi"
	if r, ok := args["resources"].(map[string]interface{}); ok {
		if c, ok := r["cpu"].(string); ok && c != "" {
			cpuLimit = c
		}
		if m, ok := r["memory"].(string); ok && m != "" {
			memLimit = m
		}
	}

	image, initScript := buildInstallInit(parsed, command)
	envVars := buildEnvVars(parsed, args)

	jobName := fmt.Sprintf("oc-%s-%d", skillName, time.Now().UnixNano())

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: e.namespace,
			Labels: map[string]string{
				"app":   "openclaw-skill",
				"skill": skillName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &e.ttlSeconds,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "skill",
							Image:           image,
							Command:         []string{"sh", "-c"},
							Args:            []string{initScript},
							Env:             envVars,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cpuLimit),
									corev1.ResourceMemory: resource.MustParse(memLimit),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "skill", MountPath: "/skill", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "skill",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: fmt.Sprintf("%s/%s", e.skillsDir, skillName),
									Type: func() *corev1.HostPathType { t := corev1.HostPathDirectory; return &t }(),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
			BackoffLimit: ptrInt32p(0),
		},
	}

	if _, err := e.k8sClient.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("failed to create openclaw job: %w", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-ctxWithTimeout.Done():
			e.k8sClient.BatchV1().Jobs(e.namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
			return nil, fmt.Errorf("openclaw skill %q timed out after %v", skillName, timeout)
		default:
		}

		j, err := e.k8sClient.BatchV1().Jobs(e.namespace).Get(ctxWithTimeout, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get job status: %w", err)
		}

		if j.Status.Succeeded > 0 {
			logs, _ := e.readJobLogs(ctx, jobName, e.namespace)
			return &ToolResult{
				Name:   e.def.Name,
				Result: map[string]interface{}{
					"status":  "success",
					"skill":   skillName,
					"command": command,
					"stdout":  logs,
					"skillInstructions": skillBody,
				},
			}, nil
		}

		if j.Status.Failed > 0 {
			logs, _ := e.readJobLogs(ctx, jobName, e.namespace)
			return &ToolResult{
				Name:   e.def.Name,
				Result: map[string]interface{}{
					"status":  "failed",
					"skill":   skillName,
					"command": command,
					"stderr":  logs,
				},
				Error: fmt.Sprintf("openclaw skill %q failed:\n%s", skillName, logs),
			}, nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}
