package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

type ChangeType string

const (
	ChangeAdded    ChangeType = "added"
	ChangeRemoved  ChangeType = "removed"
	ChangeModified ChangeType = "modified"
)

type Change struct {
	ResourceType string     `json:"resourceType"`
	Key          string     `json:"key"`
	Type         ChangeType `json:"type"`
	FromDigest   string     `json:"fromDigest,omitempty"`
	ToDigest     string     `json:"toDigest,omitempty"`
}

type Diff struct {
	FromDigest string   `json:"fromDigest"`
	ToDigest   string   `json:"toDigest"`
	Changes    []Change `json:"changes"`
}

type UpgradePlan struct {
	FromDigest           string   `json:"fromDigest"`
	ToDigest             string   `json:"toDigest"`
	TargetVersion        string   `json:"targetVersion"`
	Changes              []Change `json:"changes"`
	RollbackTargetDigest string   `json:"rollbackTargetDigest"`
	IdempotencyKey       string   `json:"idempotencyKey"`
}

type Inspection struct {
	ID                   string           `json:"id"`
	Version              string           `json:"version"`
	Digest               string           `json:"digest"`
	Agents               int              `json:"agents"`
	Teams                int              `json:"teams"`
	Objectives           int              `json:"objectives"`
	Runbooks             int              `json:"runbooks"`
	RequiredCapabilities []string         `json:"requiredCapabilities,omitempty"`
	Signatures           []string         `json:"signatures,omitempty"`
	Owners               []OwnerReference `json:"owners,omitempty"`
}

func Inspect(bundle *Bundle) (*Inspection, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	result := &Inspection{ID: bundle.Metadata.ID, Version: bundle.Metadata.Version, Digest: bundle.Digest, Agents: len(bundle.Agents), Teams: len(bundle.Teams), Objectives: len(bundle.Objectives), Runbooks: len(bundle.Runbooks)}
	for _, requirement := range bundle.Compatibility.RequiredCapabilities {
		result.RequiredCapabilities = append(result.RequiredCapabilities, requirement.ID)
	}
	for _, signature := range bundle.Signatures {
		result.Signatures = append(result.Signatures, signature.KeyID)
	}
	ownerSet := map[OwnerReference]bool{}
	for _, objective := range bundle.Objectives {
		ownerSet[objective.Owner] = true
	}
	for owner := range ownerSet {
		result.Owners = append(result.Owners, owner)
	}
	sort.Slice(result.Owners, func(i, j int) bool {
		return string(result.Owners[i].Kind)+"\x00"+result.Owners[i].Key < string(result.Owners[j].Kind)+"\x00"+result.Owners[j].Key
	})
	return result, nil
}

func Compare(from, to *Bundle) (*Diff, error) {
	if err := from.Validate(); err != nil {
		return nil, err
	}
	if err := to.Validate(); err != nil {
		return nil, err
	}
	left, err := resourceDigests(from)
	if err != nil {
		return nil, err
	}
	right, err := resourceDigests(to)
	if err != nil {
		return nil, err
	}
	result := &Diff{FromDigest: from.Digest, ToDigest: to.Digest}
	keys := map[string]bool{}
	for key := range left {
		keys[key] = true
	}
	for key := range right {
		keys[key] = true
	}
	for key := range keys {
		before, hadBefore := left[key]
		after, hasAfter := right[key]
		if hadBefore && hasAfter && before == after {
			continue
		}
		change := Change{FromDigest: before, ToDigest: after}
		for index, char := range key {
			if char == 0 {
				change.ResourceType, change.Key = key[:index], key[index+1:]
				break
			}
		}
		switch {
		case !hadBefore:
			change.Type = ChangeAdded
		case !hasAfter:
			change.Type = ChangeRemoved
		default:
			change.Type = ChangeModified
		}
		result.Changes = append(result.Changes, change)
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].ResourceType+"\x00"+result.Changes[i].Key < result.Changes[j].ResourceType+"\x00"+result.Changes[j].Key
	})
	return result, nil
}

func PlanUpgrade(current, target *Bundle) (*UpgradePlan, error) {
	if current.Metadata.ID != target.Metadata.ID {
		return nil, errors.New("workforce bundle upgrade requires the same portable identity")
	}
	diff, err := Compare(current, target)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte("openseal.bundle.upgrade.v1\x00" + current.Digest + "\x00" + target.Digest))
	return &UpgradePlan{
		FromDigest: current.Digest, ToDigest: target.Digest, TargetVersion: target.Metadata.Version,
		Changes: diff.Changes, RollbackTargetDigest: current.Digest, IdempotencyKey: "bundle-upgrade:" + hex.EncodeToString(digest[:16]),
	}, nil
}

func resourceDigests(bundle *Bundle) (map[string]string, error) {
	result := map[string]string{}
	add := func(kind, key string, value interface{}) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		result[kind+"\x00"+key] = "sha256:" + hex.EncodeToString(digest[:])
		return nil
	}
	if err := add("metadata", bundle.Metadata.ID, struct {
		Metadata      Metadata      `json:"metadata"`
		Compatibility Compatibility `json:"compatibility"`
	}{bundle.Metadata, bundle.Compatibility}); err != nil {
		return nil, err
	}
	for _, item := range bundle.Agents {
		if err := add("agent", item.Key, item); err != nil {
			return nil, err
		}
	}
	for _, item := range bundle.Teams {
		if err := add("team", item.Key, item); err != nil {
			return nil, err
		}
	}
	for _, item := range bundle.Objectives {
		if err := add("objective", item.Key, item); err != nil {
			return nil, err
		}
	}
	for _, item := range bundle.Runbooks {
		if err := add("runbook", item.Key, item); err != nil {
			return nil, err
		}
	}
	return result, nil
}
