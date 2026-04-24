package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

type RepoManager struct {
	logger    *zap.SugaredLogger
	skillsDir string
}

func NewRepoManager(logger *zap.SugaredLogger, skillsDir string) *RepoManager {
	return &RepoManager{
		logger:    logger,
		skillsDir: skillsDir,
	}
}

func (m *RepoManager) TestRepoAccess(ctx context.Context, repo RepoConfig) error {
	testDir, err := os.MkdirTemp("", "skill-repo-test-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(testDir)

	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", repo.Url)
	cmd.Dir = testDir

	if repo.AccessToken != "" || repo.Password != "" {
		askPass := m.createAskPassScript(repo)
		defer os.Remove(askPass)
		cmd.Env = append(os.Environ(), fmt.Sprintf("GIT_ASKPASS=%s", askPass))
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote failed: %s - %w", string(output), err)
	}

	return nil
}

func (m *RepoManager) SyncRepositories(ctx context.Context, repos []*RepoConfig) error {
	for _, repo := range repos {
		if err := m.syncRepo(ctx, repo); err != nil {
			m.logger.Errorw("failed to sync repo", "url", repo.Url, "error", err)
		}
	}
	return nil
}

func (m *RepoManager) syncRepo(ctx context.Context, repo *RepoConfig) error {
	repoDir := filepath.Join(m.skillsDir, "repos", repo.Name)

	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return m.cloneRepo(ctx, repo, repoDir)
	}

	return m.updateRepo(ctx, repo, repoDir)
}

func (m *RepoManager) cloneRepo(ctx context.Context, repo *RepoConfig, targetDir string) error {
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return err
	}

	branch := repo.Branch
	if branch == "" {
		branch = "main"
	}

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "-b", branch, repo.Url, targetDir)
	cmd.Dir = filepath.Dir(targetDir)

	if repo.AccessToken != "" || repo.Password != "" {
		askPass := m.createAskPassScript(*repo)
		defer os.Remove(askPass)
		cmd.Env = append(os.Environ(), fmt.Sprintf("GIT_ASKPASS=%s", askPass))
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s - %w", string(output), err)
	}

	return nil
}

func (m *RepoManager) updateRepo(ctx context.Context, repo *RepoConfig, repoDir string) error {
	branch := repo.Branch
	if branch == "" {
		branch = "main"
	}

	cmds := [][]string{
		{"git", "fetch", "origin"},
		{"git", "reset", "--hard", fmt.Sprintf("origin/%s", branch)},
	}

	for _, args := range cmds {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = repoDir

		if repo.AccessToken != "" || repo.Password != "" {
			askPass := m.createAskPassScript(*repo)
			defer os.Remove(askPass)
			cmd.Env = append(os.Environ(), fmt.Sprintf("GIT_ASKPASS=%s", askPass))
		}

		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s failed: %s - %w", strings.Join(args, " "), string(output), err)
		}
	}

	return nil
}

func (m *RepoManager) createAskPassScript(repo RepoConfig) string {
	script := "#!/bin/sh\n"
	if repo.AccessToken != "" {
		script += fmt.Sprintf("echo '%s'\n", repo.AccessToken)
	} else if repo.Password != "" {
		script += fmt.Sprintf("echo '%s'\n", repo.Password)
	} else {
		script += "echo ''\n"
	}

	tmpFile, _ := os.CreateTemp("", "git-ask-pass-*.sh")
	tmpFile.WriteString(script)
	tmpFile.Close()
	os.Chmod(tmpFile.Name(), 0755)
	return tmpFile.Name()
}
