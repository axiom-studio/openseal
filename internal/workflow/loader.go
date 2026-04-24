package workflow

import (
	"os"
	"path/filepath"
)

func LoadWorkflow(path string) (*Workflow, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	wf, err := ParseWorkflow(data, absPath)
	if err != nil {
		return nil, err
	}

	wf.SourceFile = absPath
	return wf, nil
}

// LoadWorkflowDir loads all .hcl files from a directory.
func LoadWorkflowDir(dir string) ([]*Workflow, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, err
	}

	var workflows []*Workflow
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) == ".hcl" || filepath.Ext(entry.Name()) == ".wf" {
			wf, err := LoadWorkflow(filepath.Join(absDir, entry.Name()))
			if err != nil {
				return nil, err
			}
			workflows = append(workflows, wf)
		}
	}

	return workflows, nil
}
