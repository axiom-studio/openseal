package clawhub

import "sort"

const LifecycleAPIVersion = "openseal.clawhub.lifecycle/v1"

type LifecycleOperation string

const (
	LifecycleInspectVersions LifecycleOperation = "inspect_versions"
	LifecycleInspectFiles    LifecycleOperation = "inspect_files"
	LifecycleInspectSecurity LifecycleOperation = "inspect_security"
	LifecycleInstall         LifecycleOperation = "install"
	LifecycleVerify          LifecycleOperation = "verify"
	LifecyclePin             LifecycleOperation = "pin"
	LifecycleUnpin           LifecycleOperation = "unpin"
	LifecycleUpdate          LifecycleOperation = "update"
	LifecycleUpdateAll       LifecycleOperation = "update_all"
	LifecycleUninstall       LifecycleOperation = "uninstall"
)

type LifecycleOutcome string

const (
	LifecycleOutcomeInstalled LifecycleOutcome = "installed"
	LifecycleOutcomeUpdated   LifecycleOutcome = "updated"
	LifecycleOutcomeUnchanged LifecycleOutcome = "unchanged"
	LifecycleOutcomePinned    LifecycleOutcome = "pinned"
	LifecycleOutcomeUnpinned  LifecycleOutcome = "unpinned"
	LifecycleOutcomeVerified  LifecycleOutcome = "verified"
	LifecycleOutcomeRemoved   LifecycleOutcome = "removed"
	LifecycleOutcomeSkipped   LifecycleOutcome = "skipped"
	LifecycleOutcomeError     LifecycleOutcome = "error"
)

type LifecycleErrorCode string

const (
	LifecycleErrorPinned             LifecycleErrorCode = "pinned"
	LifecycleErrorModified           LifecycleErrorCode = "locally_modified"
	LifecycleErrorVerificationFailed LifecycleErrorCode = "verification_failed"
	LifecycleErrorNotFound           LifecycleErrorCode = "not_found"
	LifecycleErrorAmbiguous          LifecycleErrorCode = "ambiguous_reference"
	LifecycleErrorCanceled           LifecycleErrorCode = "canceled"
	LifecycleErrorInvalid            LifecycleErrorCode = "invalid"
	LifecycleErrorUnavailable        LifecycleErrorCode = "unavailable"
)

// LifecycleCapability is the portable truth contract exposed by hosts. A host
// may authorize a subset, but it must not advertise operations it cannot
// execute with canonical OpenSeal semantics.
type LifecycleCapability struct {
	APIVersion string               `json:"apiVersion"`
	Operations []LifecycleOperation `json:"operations"`
}

func CanonicalLifecycleCapability() LifecycleCapability {
	return LifecycleCapability{APIVersion: LifecycleAPIVersion, Operations: []LifecycleOperation{
		LifecycleInspectVersions, LifecycleInspectFiles, LifecycleInspectSecurity,
		LifecycleInstall, LifecycleVerify, LifecyclePin, LifecycleUnpin,
		LifecycleUpdate, LifecycleUpdateAll, LifecycleUninstall,
	}}
}

// LifecycleResult is a secret-free durable/audit-safe operation result. It
// carries source identity and versions but never archive bytes, files, prompts,
// credentials, or installation paths.
type LifecycleResult struct {
	APIVersion      string             `json:"apiVersion"`
	Operation       LifecycleOperation `json:"operation"`
	SourceIdentity  string             `json:"sourceIdentity"`
	Reference       SkillReference     `json:"reference"`
	PreviousVersion string             `json:"previousVersion,omitempty"`
	Version         string             `json:"version,omitempty"`
	Outcome         LifecycleOutcome   `json:"outcome"`
	Changed         bool               `json:"changed"`
	Reason          string             `json:"reason,omitempty"`
	ErrorCode       LifecycleErrorCode `json:"errorCode,omitempty"`
	DiagnosticRef   string             `json:"diagnosticRef,omitempty"`
}

type LifecycleBatchResult struct {
	APIVersion string             `json:"apiVersion"`
	Operation  LifecycleOperation `json:"operation"`
	Results    []LifecycleResult  `json:"results"`
}

func (r *LifecycleBatchResult) Sort() {
	if r == nil {
		return
	}
	sort.Slice(r.Results, func(i, j int) bool { return r.Results[i].SourceIdentity < r.Results[j].SourceIdentity })
}
