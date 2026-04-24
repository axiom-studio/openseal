package commands

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

func skillCmd(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: openseal skill <subcommand> [options]

Subcommands:
  install   Install a skill from a git repository
  list      List installed skills

Use "openseal skill <subcommand> --help" for more information.`)
		os.Exit(1)
	}

	switch args[0] {
	case "install":
		skillInstallCmd(args[1:])
	case "list":
		skillListCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func skillInstallCmd(args []string) {
	fs := flag.NewFlagSet("skill install", flag.ExitOnError)
	skillsDir := fs.String("dir", defaultSkillsDir(), "directory for skill storage")
	branch := fs.String("branch", "main", "git branch to clone")
	help := fs.Bool("help", false, "print help for skill install")
	fs.Parse(args)

	if *help || fs.NArg() == 0 {
		fmt.Println(`Usage: openseal skill install <git-url> [options]

Install a skill from a git repository.

Arguments:
  <git-url>   Git repository URL of the skill

Options:
  --dir <path>     Directory for skill storage (default: ./skills)
  --branch <name>  Git branch to clone (default: main)
  --help           Print this help message`)
		return
	}

	repoURL := fs.Arg(0)

	logger, _ := zap.NewProduction()
	sugar := logger.Sugar()

	mgr := skill.NewRepoManager(sugar, *skillsDir)

	repo := &skill.RepoConfig{
		Name:   repoNameFromURL(repoURL),
		Url:    repoURL,
		Branch: *branch,
	}

	if err := mgr.TestRepoAccess(context.Background(), *repo); err != nil {
		sugar.Fatalf("repo access test failed: %v", err)
	}

	if err := mgr.SyncRepositories(context.Background(), []*skill.RepoConfig{repo}); err != nil {
		sugar.Fatalf("skill install failed: %v", err)
	}

	fmt.Printf("Skill installed from %s (branch: %s)\n", repoURL, *branch)
}

func skillListCmd(args []string) {
	fs := flag.NewFlagSet("skill list", flag.ExitOnError)
	skillsDir := fs.String("dir", defaultSkillsDir(), "directory for skill storage")
	help := fs.Bool("help", false, "print help for skill list")
	fs.Parse(args)

	if *help {
		fmt.Println(`Usage: openseal skill list [options]

List installed skills.

Options:
  --dir <path>  Directory for skill storage (default: ./skills)
  --help        Print this help message`)
		return
	}

	reposDir := filepath.Join(*skillsDir, "repos")
	entries, err := os.ReadDir(reposDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No skills installed.")
			return
		}
		fmt.Fprintf(os.Stderr, "failed to read skills dir: %v\n", err)
		os.Exit(1)
	}

	if len(entries) == 0 {
		fmt.Println("No skills installed.")
		return
	}

	fmt.Printf("%-30s %s\n", "NAME", "SOURCE")
	fmt.Println(string(make([]byte, 50)))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(reposDir, entry.Name(), "skill.yaml")
		name := entry.Name()
		source := "git"
		if data, err := os.ReadFile(manifestPath); err == nil {
			var manifest struct {
				Name string `yaml:"name"`
			}
			if json.Unmarshal(data, &manifest) == nil && manifest.Name != "" {
				name = manifest.Name
			}
		}
		fmt.Printf("%-30s %s\n", name, source)
	}
}

func defaultSkillsDir() string {
	if dir := os.Getenv("OPENSEAL_SKILLS_DIR"); dir != "" {
		return dir
	}
	return "./skills"
}

func repoNameFromURL(url string) string {
	base := filepath.Base(url)
	ext := filepath.Ext(base)
	if ext != "" {
		return base[:len(base)-len(ext)]
	}
	return base
}