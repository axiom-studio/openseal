package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent/skill"
	"github.com/go-pg/pg"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Config holds Hermes configuration
type Config struct {
	NATSURL          string
	ClusterID        int
	SkillsDir        string
	OpenSealReloadURL   string
	SentinelAPIURL   string
	SystemSkillRepos string
	OpenSealID          string
	UseHTTPOnly      bool
	CortexAPIURL     string
	// Database config
	PostgresHost     string
	PostgresPort     string
	PostgresUser     string
	PostgresPassword string
	PostgresDatabase string
}

// SkillRepo represents a skill repository from the database
type SkillRepo struct {
	Id        int
	Name      string
	Url       string
	IsEnabled bool
}

// Hermes manages and runs external skills for OpenSeal
// Named after the messenger god who delivers and manages
type Hermes struct {
	config     *Config
	logger     *zap.SugaredLogger
	natsClient *nats.Conn
	db         *pg.DB
	processes  map[string]*exec.Cmd // skillID -> running process
}

func main() {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	sugar := logger.Sugar()

	// Load config
	config := &Config{
		NATSURL:          getEnv("NATS_URL", "nats://localhost:4222"),
		ClusterID:        1,
		SkillsDir:        getEnv("SKILLS_DIR", "/var/lib/axiom/skills"),
		OpenSealReloadURL:   getEnv("OPENSEAL_RELOAD_URL", "http://localhost:8081/internal/reload"),
		SentinelAPIURL:   getEnv("SENTINEL_API_URL", "http://sentinel:80"),
		SystemSkillRepos: getEnv("SYSTEM_SKILL_REPOS", ""),
		OpenSealID:          getEnv("OPENSEAL_ID", ""),
		UseHTTPOnly:      getEnv("USE_HTTP_ONLY", "false") == "true",
		CortexAPIURL:     getEnv("CORTEX_API_URL", "http://cortex:80/orchestrator"),
		PostgresHost:     getEnv("POSTGRES_HOST", "postgresql-postgresql"),
		PostgresPort:     getEnv("POSTGRES_PORT", "5432"),
		PostgresUser:     getEnv("POSTGRES_USER", "postgres"),
		PostgresPassword: getEnv("POSTGRES_PASSWORD", "postgres"),
		PostgresDatabase: getEnv("POSTGRES_DATABASE", "axiom"),
	}

	sugar.Infow("starting skill builder", "config", config)

	// Connect to NATS
	nc, err := nats.Connect(config.NATSURL)
	if err != nil {
		sugar.Fatalw("failed to connect to NATS", "error", err)
	}
	defer nc.Close()

	// Connect to PostgreSQL
	db := pg.Connect(&pg.Options{
		Addr:     fmt.Sprintf("%s:%s", config.PostgresHost, config.PostgresPort),
		User:     config.PostgresUser,
		Password: config.PostgresPassword,
		Database: config.PostgresDatabase,
	})
	defer db.Close()

	builder := &Hermes{
		config:    config,
		logger:    sugar,
		db:        db,
		processes: make(map[string]*exec.Cmd),
	}

	// Clone skill repos and build catalog on startup (do NOT start processes)
	builder.cloneSkillReposForCatalog()

	// Restore installed skills (user clicked install before restart)
	builder.restoreInstalledSkills()

	// Subscribe to skill install events
	skillsInstallSubject := "agent.skills.install"
	_, err = nc.Subscribe(skillsInstallSubject, func(msg *nats.Msg) {
		sugar.Infow("received skill install event")

		var req skill.SkillInstallRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			sugar.Errorw("failed to parse skill install request", "error", err)
			return
		}

		sugar.Infow("building skill repositories", "repoCount", len(req.RepoURLs), "skillCount", len(req.SkillIDs))

		for _, repoURL := range req.RepoURLs {
			if err := builder.buildAndStartSkill(repoURL); err != nil {
				sugar.Errorw("failed to build skill", "url", repoURL, "error", err)
			}
		}

		for _, skillID := range req.SkillIDs {
			if err := builder.buildAndStartSkillByID(skillID); err != nil {
				sugar.Errorw("failed to build skill by ID", "skillId", skillID, "error", err)
			}
		}

		// Ack the message
		msg.Ack()
	})
	if err != nil {
		sugar.Fatalw("failed to subscribe to skill install events", "error", err)
	}

	sugar.Infow("subscribed to skill install events", "subject", skillsInstallSubject)

	skillsUninstallSubject := "agent.skills.uninstall"
	_, err = nc.Subscribe(skillsUninstallSubject, func(msg *nats.Msg) {
		skillID := string(msg.Data)
		sugar.Infow("received skill uninstall event", "skillId", skillID)
		builder.uninstallSkill(skillID)
		msg.Ack()
	})
	if err != nil {
		sugar.Fatalw("failed to subscribe to skill uninstall events", "error", err)
	}

	sugar.Infow("subscribed to skill uninstall events", "subject", skillsUninstallSubject)

	// Keep running
	select {}
}

// cloneSkillReposForCatalog queries enabled skill repos and clones them for catalog discovery only.
// Does NOT build or start any skills — that happens on install via NATS event.
func (b *Hermes) cloneSkillReposForCatalog() {
	var repos []SkillRepo
	_, err := b.db.Query(&repos, `SELECT id, name, url, is_enabled FROM agent_skill_repositories WHERE is_enabled = true`)
	if err != nil {
		b.logger.Warnw("failed to query skill repositories from database", "error", err)
		return
	}

	if len(repos) == 0 {
		b.logger.Infow("no enabled skills found in database")
		return
	}

	b.logger.Infow("cloning skill repos for catalog", "count", len(repos))

	for _, repo := range repos {
		repoName := filepath.Base(repo.Url)
		if strings.HasSuffix(repoName, ".git") {
			repoName = strings.TrimSuffix(repoName, ".git")
		}
		skillDir := filepath.Join(b.config.SkillsDir, repoName)
		if err := b.cloneRepo(repo.Url, skillDir); err != nil {
			b.logger.Errorw("failed to clone skill repo for catalog", "name", repo.Name, "error", err)
		}
	}
}

// InstalledSkill represents a skill the user has installed.
type InstalledSkill struct {
	SkillID string
	Version string
}

// restoreInstalledSkills queries agent_skills for enabled (installed) skills
// and builds/starts their processes. Only runs for skills the user explicitly installed.
// If OPENSEAL_ID is set and assignments exist, only restores assigned skills.
func (b *Hermes) restoreInstalledSkills() {
	var skills []InstalledSkill
	var err error

	if b.config.OpenSealID != "" {
		skills, err = b.getAssignedSkillsForOpenSeal()
		if err != nil {
			b.logger.Warnw("failed to get assigned skills, falling back to all enabled", "opensealId", b.config.OpenSealID, "error", err)
			_, err = b.db.Query(&skills, `SELECT skill_id, version FROM agent_skills WHERE is_enabled = true`)
		}
	} else {
		_, err = b.db.Query(&skills, `SELECT skill_id, version FROM agent_skills WHERE is_enabled = true`)
	}

	if err != nil {
		b.logger.Warnw("failed to query installed skills", "error", err)
		return
	}

	if len(skills) == 0 {
		b.logger.Infow("no installed skills to restore")
		return
	}

	b.logger.Infow("restoring installed skills", "count", len(skills), "opensealId", b.config.OpenSealID)

	for _, s := range skills {
		if err := b.buildAndStartSkillByID(s.SkillID); err != nil {
			b.logger.Errorw("failed to restore skill", "skillId", s.SkillID, "error", err)
		}
	}
}

// getAssignedSkillsForOpenSeal returns skills assigned to this OpenSeal instance.
// For out-of-cluster instances (USE_HTTP_ONLY), polls the Cortex API.
// For in-cluster instances, queries the database directly.
func (b *Hermes) getAssignedSkillsForOpenSeal() ([]InstalledSkill, error) {
	if b.config.UseHTTPOnly {
		return b.getAssignedSkillsViaHTTP()
	}
	var skills []InstalledSkill
	_, err := b.db.Query(&skills, `
		SELECT s.skill_id, s.version 
		FROM agent_skills s
		JOIN agent_skill_runtime_assignments a ON a.skill_id = s.skill_id
		WHERE a.atlas_id = ? AND a.is_enabled = true AND s.is_enabled = true
	`, b.config.OpenSealID)
	return skills, err
}

// getAssignedSkillsViaHTTP polls Cortex API for assigned skills.
// Used by out-of-cluster OpenSeal instances that don't have DB access.
func (b *Hermes) getAssignedSkillsViaHTTP() ([]InstalledSkill, error) {
	url := fmt.Sprintf("%s/agent/skills/assigned?opensealId=%s", b.config.CortexAPIURL, b.config.OpenSealID)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch assigned skills: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch assigned skills: status %d", resp.StatusCode)
	}

	var result struct {
		Result []struct {
			SkillId string `json:"skillId"`
			Version string `json:"version"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode assigned skills: %w", err)
	}

	var skills []InstalledSkill
	for _, s := range result.Result {
		skills = append(skills, InstalledSkill{SkillID: s.SkillId, Version: s.Version})
	}
	return skills, nil
}

// buildAndStartSkillByID finds a specific skill by its ID in already-cloned repos,
// builds and starts only that skill.
func (b *Hermes) buildAndStartSkillByID(skillID string) error {
	b.logger.Infow("building skill by ID", "skillId", skillID)

	yamlPath, repoDir, err := b.findSkillYamlByID(skillID)
	if err != nil {
		return fmt.Errorf("skill %s not found in cloned repos: %w", skillID, err)
	}

	return b.buildAndStartSkillFromManifest(yamlPath, repoDir)
}

// findSkillYamlByID searches cloned repos for a skill.yaml with the given skill ID.
func (b *Hermes) findSkillYamlByID(skillID string) (yamlPath, repoDir string, err error) {
	entries, err := os.ReadDir(b.config.SkillsDir)
	if err != nil {
		return "", "", err
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		repoDir := filepath.Join(b.config.SkillsDir, entry.Name())
		yamlPaths, err := b.discoverSkillYamlFiles(repoDir)
		if err != nil {
			continue
		}
		for _, yamlPath := range yamlPaths {
			data, err := os.ReadFile(yamlPath)
			if err != nil {
				continue
			}
			var manifest skill.SkillManifest
			if err := yaml.Unmarshal(data, &manifest); err != nil {
				continue
			}
			if manifest.Metadata.ID == skillID {
				return yamlPath, repoDir, nil
			}
		}
	}

	return "", "", fmt.Errorf("skill %s not found", skillID)
}

// buildAndStartSkill clones a skill repo, builds it, and starts it if it's a gRPC skill.
// Supports both single-repo skills (one skill.yaml at root) and monorepo skills (multiple skill.yaml files).
func (b *Hermes) buildAndStartSkill(repoURL string) error {
	b.logger.Infow("building skill from repo", "url", repoURL)

	// Extract repo name for directory
	repoName := filepath.Base(repoURL)
	if strings.HasSuffix(repoName, ".git") {
		repoName = strings.TrimSuffix(repoName, ".git")
	}
	skillDir := filepath.Join(b.config.SkillsDir, repoName)

	// Clone the repo
	if err := b.cloneRepo(repoURL, skillDir); err != nil {
		return fmt.Errorf("failed to clone repo: %w", err)
	}

	// Discover all skill.yaml files in the repo
	skillYamlPaths, err := b.discoverSkillYamlFiles(skillDir)
	if err != nil {
		return fmt.Errorf("failed to discover skill.yaml files: %w", err)
	}

	b.logger.Infow("discovered skill.yaml files", "count", len(skillYamlPaths), "repo", repoName)

	if len(skillYamlPaths) == 0 {
		return fmt.Errorf("no skill.yaml files found in repo: %s", repoURL)
	}

	// Process each skill.yaml found
	for _, yamlPath := range skillYamlPaths {
		if err := b.buildAndStartSkillFromManifest(yamlPath, skillDir); err != nil {
			b.logger.Errorw("failed to build/start skill", "manifest", yamlPath, "error", err)
			// Continue to next skill instead of failing entirely
		}
	}

	return nil
}

// discoverSkillYamlFiles finds all skill.yaml files in a directory tree
func (b *Hermes) discoverSkillYamlFiles(rootDir string) ([]string, error) {
	var yamlPaths []string

	err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip .git directory and hidden directories
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		// Skip vendor directory
		if d.IsDir() && d.Name() == "vendor" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "skill.yaml" {
			yamlPaths = append(yamlPaths, path)
		}
		return nil
	})

	return yamlPaths, err
}

// buildAndStartSkillFromManifest builds and starts a single skill from its manifest path
func (b *Hermes) buildAndStartSkillFromManifest(manifestPath, repoDir string) error {
	// Read skill manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read skill.yaml: %w", err)
	}

	var manifest skill.SkillManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("failed to parse skill.yaml: %w", err)
	}

	skillID := manifest.Metadata.ID

	// Only support gRPC skills
	if manifest.Spec.ExecutorType != "grpc" {
		b.logger.Infow("skill is not gRPC type, skipping", "skill", skillID, "type", manifest.Spec.ExecutorType)
		return nil
	}

	// Determine skill directory (parent of skill.yaml)
	skillDir := filepath.Dir(manifestPath)

	// Check if binary already exists
	platform := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	binaryRelPath := manifest.Spec.GRPC.Binary[platform]
	if binaryRelPath == "" {
		b.logger.Infow("no binary path in skill.yaml for platform, skipping", "skill", skillID, "platform", platform)
		return nil
	}
	binaryPath := filepath.Join(skillDir, binaryRelPath)

	binaryExists := false
	if _, err := os.Stat(binaryPath); err == nil {
		binaryExists = true
		b.logger.Infow("skill binary already exists, skipping build", "binary", binaryPath)
	}

	// Build the gRPC skill binary if it doesn't exist
	if !binaryExists {
		if err := b.buildGRPCSkill(skillDir, repoDir, binaryRelPath, &manifest); err != nil {
			return fmt.Errorf("failed to build gRPC skill: %w", err)
		}
	}

	// Start the gRPC skill process
	if err := b.startGRPCSkill(skillDir, &manifest); err != nil {
		return fmt.Errorf("failed to start gRPC skill: %w", err)
	}

	b.logger.Infow("successfully built and started gRPC skill", "skill", skillID)
	return nil
}

// cloneRepo clones a git repository
func (b *Hermes) cloneRepo(url, dir string) error {
	// Check if already cloned (look for .git in the directory)
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		b.logger.Infow("repo already exists, pulling latest", "dir", dir)
		// Git pull instead
		cmd := exec.Command("git", "-C", dir, "pull", "origin", "main")
		if output, err := cmd.CombinedOutput(); err != nil {
			b.logger.Warnw("git pull failed, re-cloning", "error", err, "output", string(output))
			os.RemoveAll(dir)
		} else {
			return nil
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Clone the repo using git CLI directly
	b.logger.Infow("cloning skill repo", "url", url, "dir", dir)

	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", "main", url, dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s - %w", string(output), err)
	}

	b.logger.Infow("successfully cloned repo", "dir", dir)
	return nil
}

// buildGRPCSkill builds a gRPC skill binary
func (b *Hermes) buildGRPCSkill(skillDir, repoDir, binaryRelPath string, manifest *skill.SkillManifest) error {
	skillID := manifest.Metadata.ID

	// Determine output path
	outputPath := filepath.Join(skillDir, binaryRelPath)

	b.logger.Infow("building gRPC skill binary", "skill", skillID, "output", outputPath)

	// Check for go.mod: prefer skillDir, fall back to repoDir
	goModDir := skillDir
	if _, err := os.Stat(filepath.Join(skillDir, "go.mod")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err == nil {
			goModDir = repoDir
		}
	}

	goModPath := filepath.Join(goModDir, "go.mod")
	if _, err := os.Stat(goModPath); os.IsNotExist(err) {
		return fmt.Errorf("skill must have go.mod: %s", goModPath)
	}

	// Download dependencies
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = goModDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod tidy failed: %s - %w", string(output), err)
	}

	// Build the binary
	cmd = exec.Command("go", "build", "-buildvcs=false", "-o", outputPath, ".")
	cmd.Dir = goModDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		fmt.Sprintf("GOOS=%s", runtime.GOOS),
		fmt.Sprintf("GOARCH=%s", runtime.GOARCH),
	)

	b.logger.Infow("running go build", "dir", repoDir)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}

	// Make executable
	if err := os.Chmod(outputPath, 0755); err != nil {
		return fmt.Errorf("failed to make binary executable: %w", err)
	}

	b.logger.Infow("gRPC skill binary built successfully", "output", outputPath, "size", fileSize(outputPath))
	return nil
}

// startGRPCSkill starts a gRPC skill process
func (b *Hermes) startGRPCSkill(repoDir string, manifest *skill.SkillManifest) error {
	skillID := manifest.Metadata.ID

	// Check if already running
	if _, ok := b.processes[skillID]; ok {
		b.logger.Infow("skill already running, stopping old process", "skill", skillID)
		b.stopSkill(skillID)
	}

	// Find binary
	platform := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	binaryFilename := fmt.Sprintf("%s-%s", skillID, platform)
	binaryPath := filepath.Join(repoDir, binaryFilename)

	// Check if binary exists
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		// Try without platform suffix
		binaryPath = filepath.Join(repoDir, skillID)
		if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
			return fmt.Errorf("skill binary not found: %s", binaryPath)
		}
	}

	// Request port from OpenSeal (centralized port management)
	port, err := requestPortFromOpenSeal(skillID)
	if err != nil {
		b.logger.Warnw("failed to request port from OpenSeal, using fallback",
			"skill", skillID, "error", err)
		// Fallback to manual port assignment
		port = 50051 + len(b.processes)
	}

	// OpenSeal registration URL for skill to phone home
	opensealURL := os.Getenv("OPENSEAL_URL")
	if opensealURL == "" {
		opensealURL = "http://localhost:8081"
	}

	// Start the process
	cmd := exec.Command(binaryPath)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("SKILL_PORT=%d", port),
		fmt.Sprintf("SKILL_ID=%s", skillID),
		fmt.Sprintf("OPENSEAL_URL=%s", opensealURL),
	)

	// Capture output
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		// Release port back to OpenSeal if startup failed
		releasePortToOpenSeal(skillID, port)
		return fmt.Errorf("failed to start skill process: %w", err)
	}

	b.processes[skillID] = cmd

	b.logger.Infow("started gRPC skill process",
		"skill", skillID,
		"pid", cmd.Process.Pid,
		"port", port,
		"binary", binaryPath,
		"opensealURL", opensealURL)

	return nil
}

// stopSkill stops a running skill process and releases its port
func (b *Hermes) stopSkill(skillID string) {
	cmd, ok := b.processes[skillID]
	if !ok {
		return
	}

	// Get the port from the process environment to release it
	var port int
	if cmd.Env != nil {
		for _, env := range cmd.Env {
			if strings.HasPrefix(env, "SKILL_PORT=") {
				fmt.Sscanf(env[len("SKILL_PORT="):], "%d", &port)
				break
			}
		}
	}

	if cmd.Process != nil {
		cmd.Process.Signal(os.Interrupt)
		time.Sleep(2 * time.Second)
		cmd.Process.Kill()
	}

	delete(b.processes, skillID)

	// Release port back to OpenSeal
	if port > 0 {
		if err := releasePortToOpenSeal(skillID, port); err != nil {
			b.logger.Warnw("failed to release port to OpenSeal", "skill", skillID, "port", port, "error", err)
		} else {
			b.logger.Infow("released port to OpenSeal", "skill", skillID, "port", port)
		}
	}

	b.logger.Infow("stopped skill process", "skill", skillID)
}

func (b *Hermes) uninstallSkill(skillID string) {
	b.stopSkill(skillID)

	entries, err := os.ReadDir(b.config.SkillsDir)
	if err != nil {
		b.logger.Warnw("failed to read skills dir for uninstall", "skill", skillID, "error", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoDir := filepath.Join(b.config.SkillsDir, entry.Name())
		yamlPaths, err := b.discoverSkillYamlFiles(repoDir)
		if err != nil {
			continue
		}
		for _, yamlPath := range yamlPaths {
			data, err := os.ReadFile(yamlPath)
			if err != nil {
				continue
			}
			var manifest skill.SkillManifest
			if err := yaml.Unmarshal(data, &manifest); err != nil {
				continue
			}
			if manifest.Metadata.ID == skillID {
				skillDir := filepath.Dir(yamlPath)
				if err := os.RemoveAll(skillDir); err != nil {
					b.logger.Errorw("failed to remove skill directory", "skill", skillID, "dir", skillDir, "error", err)
				} else {
					b.logger.Infow("removed skill directory", "skill", skillID, "dir", skillDir)
				}
				return
			}
		}
	}

	b.logger.Infow("skill directory not found for uninstall", "skill", skillID)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func fileSize(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d bytes", info.Size())
}

// requestPortFromOpenSeal requests a port from OpenSeal for a skill
func requestPortFromOpenSeal(skillID string) (int, error) {
	opensealURL := os.Getenv("OPENSEAL_URL")
	if opensealURL == "" {
		opensealURL = "http://localhost:8081"
	}

	reqBody := map[string]string{
		"skillId": skillID,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/internal/ports/lease", opensealURL)
	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return 0, fmt.Errorf("failed to request port from OpenSeal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]string
		json.NewDecoder(resp.Body).Decode(&errResp)
		errMsg := "unknown error"
		if errResp != nil && errResp["error"] != "" {
			errMsg = errResp["error"]
		}
		return 0, fmt.Errorf("OpenSeal returned status %d: %s", resp.StatusCode, errMsg)
	}

	var leaseResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&leaseResp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}

	portFloat, ok := leaseResp["port"].(float64)
	if !ok {
		return 0, fmt.Errorf("invalid port in response")
	}

	return int(portFloat), nil
}

// releasePortToOpenSeal releases a port back to OpenSeal
func releasePortToOpenSeal(skillID string, port int) error {
	opensealURL := os.Getenv("OPENSEAL_URL")
	if opensealURL == "" {
		opensealURL = "http://localhost:8081"
	}

	reqBody := map[string]interface{}{
		"skillId": skillID,
		"port":    port,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/internal/ports/release", opensealURL)
	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("failed to release port to OpenSeal: %w", err)
	}
	defer resp.Body.Close()

	return nil
}
