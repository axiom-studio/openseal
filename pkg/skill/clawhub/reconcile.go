package clawhub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const lifecycleIntentVersion = 1

type lifecycleIntent struct {
	Version  int        `json:"version"`
	Kind     string     `json:"kind"`
	Identity string     `json:"identity"`
	Target   string     `json:"target"`
	Stage    string     `json:"stage,omitempty"`
	Backup   string     `json:"backup,omitempty"`
	Trash    string     `json:"trash,omitempty"`
	Desired  *LockEntry `json:"desired,omitempty"`
}

func (m *InstallManager) intentPath() string {
	return filepath.Join(m.workspace, ".clawhub", "lifecycle-intent.json")
}

func (m *InstallManager) writeIntent(intent lifecycleIntent) error {
	intent.Version = lifecycleIntentVersion
	return writeAtomicJSON(m.intentPath(), intent)
}

func (m *InstallManager) clearIntent() error {
	err := os.Remove(m.intentPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(m.intentPath()))
}

// ReconcileLifecycle repairs an interrupted install, update, or uninstall.
// The lockfile is the commit record: matching desired state is completed;
// otherwise filesystem changes are rolled back. It is safe to replay.
func (m *InstallManager) ReconcileLifecycle() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := m.lockWorkspace()
	if err != nil {
		return err
	}
	defer release()
	return m.reconcileLifecycleLocked()
}

func (m *InstallManager) reconcileLifecycleLocked() error {
	content, err := os.ReadFile(m.intentPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var intent lifecycleIntent
	if err := json.Unmarshal(content, &intent); err != nil {
		return fmt.Errorf("parse ClawHub lifecycle intent: %w", err)
	}
	if err := m.validateIntent(intent); err != nil {
		return err
	}
	lock, err := m.readLockfile()
	if err != nil {
		return err
	}
	entry, exists := lock.Skills[intent.Identity]
	committed := intent.Kind == "install" && exists && intent.Desired != nil && lockEntriesEqual(entry, *intent.Desired) || intent.Kind == "uninstall" && !exists
	if committed {
		for _, path := range []string{intent.Stage, intent.Backup, intent.Trash} {
			if path != "" {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
		}
		return m.clearIntent()
	}
	if intent.Kind == "install" {
		if intent.Backup != "" {
			if _, err := os.Stat(intent.Backup); err == nil {
				if err := os.RemoveAll(intent.Target); err != nil {
					return err
				}
				if err := os.Rename(intent.Backup, intent.Target); err != nil {
					return err
				}
			}
		} else if !exists {
			if err := os.RemoveAll(intent.Target); err != nil {
				return err
			}
		}
		if intent.Stage != "" {
			if err := os.RemoveAll(intent.Stage); err != nil {
				return err
			}
		}
	} else if intent.Trash != "" {
		if _, err := os.Stat(intent.Trash); err == nil {
			if _, targetErr := os.Stat(intent.Target); os.IsNotExist(targetErr) {
				if err := os.Rename(intent.Trash, intent.Target); err != nil {
					return err
				}
			} else if targetErr != nil {
				return targetErr
			}
		}
	}
	return m.clearIntent()
}

func (m *InstallManager) validateIntent(intent lifecycleIntent) error {
	if intent.Version != lifecycleIntentVersion || intent.Identity == "" || intent.Target == "" || intent.Kind != "install" && intent.Kind != "uninstall" {
		return errors.New("unsupported ClawHub lifecycle intent")
	}
	for _, path := range []string{intent.Target, intent.Stage, intent.Backup, intent.Trash} {
		if path == "" {
			continue
		}
		rel, err := filepath.Rel(m.skillsDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) && rel != "." {
			return errors.New("ClawHub lifecycle intent escapes skills directory")
		}
	}
	return nil
}

func lockEntriesEqual(left, right LockEntry) bool {
	if left.Version == nil || right.Version == nil {
		return left.Version == nil && right.Version == nil && left.InstalledAt == right.InstalledAt && left.Registry == right.Registry && left.OwnerHandle == right.OwnerHandle && left.Slug == right.Slug && left.Directory == right.Directory && left.Pinned == right.Pinned && left.PinReason == right.PinReason
	}
	return *left.Version == *right.Version && left.InstalledAt == right.InstalledAt && left.Registry == right.Registry && left.OwnerHandle == right.OwnerHandle && left.Slug == right.Slug && left.Directory == right.Directory && left.Pinned == right.Pinned && left.PinReason == right.PinReason
}
