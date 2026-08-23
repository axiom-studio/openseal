package workspace

import (
	"errors"
	"strings"
)

// Access is the framework-owned access level granted to one hosted Agent turn.
// It is not a Skill permission: the host derives it from the Agent deployment
// and enforces it when scheduling work against the Workspace.
type Access string

const (
	AccessReadOnly  Access = "read_only"
	AccessReadWrite Access = "read_write"
)

// Authority is an immutable, credential-free grant from the OpenSeal kernel to
// a trusted Agent host. Storage handles, mount paths, cluster identities, and
// credentials remain host-owned and are never part of this portable contract.
type Authority struct {
	Workspace Spec   `json:"workspace"`
	Access    Access `json:"access"`
}

func (a Authority) Validate() error {
	if err := a.Workspace.Validate(); err != nil {
		return err
	}
	switch a.Access {
	case AccessReadOnly, AccessReadWrite:
		return nil
	default:
		return errors.New("workspace authority access is invalid")
	}
}

// Select returns the deployment's exact default Workspace. Callers must not
// silently select another Workspace because the default identity is durable
// Agent state and determines the host-side storage authority.
func Select(defaultID string, workspaces []Spec) (*Spec, error) {
	defaultID = strings.TrimSpace(defaultID)
	if defaultID == "" {
		return nil, nil
	}
	for index := range workspaces {
		if workspaces[index].ID == defaultID {
			selected := workspaces[index]
			return &selected, nil
		}
	}
	return nil, errors.New("default workspace does not exist")
}
