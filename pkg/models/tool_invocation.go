package models

import (
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/toolfile"
)

// ToolInvocation is the complete, tenant-scoped transport envelope. Resolved
// credentials are deliberately separate from model-visible arguments and must
// never be persisted or logged by an invoker.
type ToolInvocation struct {
	Name  string
	Scope skill.ScopeReference
	// DeploymentID is the Skill binding authority owner. For Team-owned
	// bindings it remains the Team deployment throughout policy, credential,
	// idempotency, and audit handling.
	DeploymentID string
	// ExecutionDeploymentID is the kernel-derived Agent deployment that
	// supplies placement for this invocation. It is never model writable.
	ExecutionDeploymentID string
	SkillID               string
	SkillVersion          string
	// SkillSourceIdentity and SkillDefinitionDigest identify the trusted bound
	// definition independently of model arguments and binding configuration.
	SkillSourceIdentity   string
	SkillDefinitionDigest string
	Action                string
	ActionCallID          string
	RunID                 string
	Arguments             map[string]interface{}
	// FileArguments name the Arguments that are file inputs: each holds a
	// file's path in the Run's chat, which the host replaces with a
	// fileinput.File before calling the Skill.
	FileArguments []string
	// FileReferences are the {"$file": path} values of Arguments, where each
	// is in Arguments and the encoding its field takes. The host puts each
	// file's content there just before calling the tool.
	FileReferences []toolfile.Reference
	// OutputSchema is the action's declared output schema: a host takes the
	// file content fields it declares out of the result and keeps them as
	// chat files.
	OutputSchema map[string]interface{}
	// BindingConfig is trusted, non-secret configuration selected by the host
	// when the Skill was bound. It is never writable by the model.
	BindingConfig        map[string]interface{}
	Credentials          map[string]string
	CredentialLease      *SignedActionCredentialLease
	CredentialReferences map[string]skill.CredentialReference
	PreparedRuntime      *skill.PreparedRuntime
}
