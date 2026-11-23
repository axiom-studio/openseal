// Package git defines openseal.git, the Skill git.seal acts as: real git in
// a chat's secure VM against https://git.seal/github.com/<owner>/<repo>.git.
// Its actions are host-only. The model never calls them; the host proposes
// each clone, fetch or push as a nested host call (NestedHostActionRequest
// with SkillVersion), so policy, approvals and routine grants apply to it as
// to any action. The host binds it per run next to a connected app whose
// sign-in may be used for git, and fills access from that connection.
package git

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Any change to an action's text, schema or risk must bump SkillVersion:
// bundled definitions are immutable per version.
const (
	SkillID      = "openseal.git"
	SkillVersion = "1.0.0"

	// Fetch is one clone, fetch or pull of a repository: a read.
	Fetch = "fetch"
	// Push is one push of one branch to an exact commit: a write.
	Push = "push"

	// CredentialAccess is the slot holding the connected app's sign-in.
	CredentialAccess = mcp.CredentialAccess

	// NamePattern is a GitHub owner or repository name, ObjectPattern a
	// commit id, and BranchPattern a branch name.
	NamePattern   = `^[A-Za-z0-9_.-]{1,100}$`
	ObjectPattern = `^(?:[0-9a-f]{40}|[0-9a-f]{64})$`
	BranchPattern = `^[A-Za-z0-9._/@+-]{1,255}$`

	// Timeout bounds a call's host lease between requests.
	Timeout = 10 * time.Minute
)

func object(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func pattern(value string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": value}
}

func output() map[string]interface{} {
	return object([]interface{}{"summary", "repository"}, map[string]interface{}{
		"summary":    map[string]interface{}{"type": "string"},
		"repository": map[string]interface{}{"type": "string"},
		"bytes":      map[string]interface{}{"type": "integer", "minimum": 0},
		"branch":     pattern(BranchPattern),
		"sha":        pattern(ObjectPattern),
	})
}

// SkillDefinition is the immutable contract of openseal.git.
func SkillDefinition() *skill.Definition {
	credentials := []skill.CredentialRequirement{{Name: CredentialAccess, Kind: mcp.CredentialKind}}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Git",
		Description: "Clone, fetch and push GitHub repositories with git in the chat's secure VM, using the connected GitHub app's sign-in.",
		Icon:        "git-branch", Category: "integrations", Tags: []string{"git", "github", "repository", "clone", "push"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{
			Fetch: {
				Name: Fetch, Description: "Clone or fetch a GitHub repository with git in the chat's secure VM.",
				InputSchema:  object([]interface{}{"owner", "repository"}, map[string]interface{}{"owner": pattern(NamePattern), "repository": pattern(NamePattern)}),
				OutputSchema: output(), SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
				Credentials: credentials, Timeout: capability.Duration(Timeout), Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, HostOnly: true,
			},
			Push: {
				Name: Push, Description: "Push a branch other than the default branch to a GitHub repository with git in the chat's secure VM.",
				InputSchema: object([]interface{}{"owner", "repository", "branch", "sha"}, map[string]interface{}{
					"owner": pattern(NamePattern), "repository": pattern(NamePattern), "branch": pattern(BranchPattern), "sha": pattern(ObjectPattern),
				}),
				OutputSchema: output(), SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
				Credentials: credentials, Timeout: capability.Duration(Timeout), Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, HostOnly: true,
			},
		},
	}
}
