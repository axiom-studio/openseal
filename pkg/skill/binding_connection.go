package skill

// Skills whose bindings always stand for a connection. pkg/mcp, pkg/computer
// and the runtime own these IDs; they are repeated here because skill cannot import
// either.
const (
	mcpConnectionSkillID        = "openseal.mcp"
	externalConversationSkillID = "openseal.external-conversation"
	computerSkillID             = "openseal.computer"
)

// BindingHoldsConnection reports whether b carries a connection: credentials,
// configuration, conversation or callback adapters, or a connection Skill.
// It is the only definition of a connection. Host-supplied bindings (MCP
// servers and connected apps) are connections too; activation leaves them
// out wherever this predicate leaves stored connections out.
func BindingHoldsConnection(b *Binding) bool {
	if b == nil {
		return false
	}
	return len(b.Credentials) > 0 || len(b.Config) > 0 || len(b.EnabledConversationAdapters) > 0 ||
		len(b.EnabledCallbackAdapters) > 0 || b.SkillID == mcpConnectionSkillID || b.SkillID == externalConversationSkillID ||
		b.SkillID == computerSkillID
}

// PersonCredentialReferenceID is the reference ID of a per_person slot: the
// slot is filled at run time by the acting person's own sign-in instead of a
// stored credential.
const PersonCredentialReferenceID = "person://sign-in"

// IsPersonCredentialReference reports whether ref is a per_person slot.
func IsPersonCredentialReference(ref CredentialReference) bool {
	return ref.ID == PersonCredentialReferenceID
}

// BindingUsesPersonCredentials reports whether any slot of b is per_person.
func BindingUsesPersonCredentials(b *Binding) bool {
	if b == nil {
		return false
	}
	for _, ref := range b.Credentials {
		if IsPersonCredentialReference(ref) {
			return true
		}
	}
	return false
}

// PersonCredentialAllowed reports whether slot of b may hold the person ref:
// every requirement of definition that uses slot is an action credential with
// OAuth2 != nil, and b enables no conversation or callback adapter.
func PersonCredentialAllowed(definition *Definition, b *Binding, slot string) bool {
	if definition == nil || b == nil || slot == "" || len(b.EnabledConversationAdapters) > 0 || len(b.EnabledCallbackAdapters) > 0 {
		return false
	}
	if definition.Prompt != nil && requirementsUseSlot(definition.Prompt.Credentials, slot) {
		return false
	}
	for _, adapter := range definition.ConversationAdapters {
		if requirementsUseSlot(adapter.Credentials, slot) {
			return false
		}
	}
	for _, adapter := range definition.CallbackAdapters {
		if requirementsUseSlot(adapter.Credentials, slot) {
			return false
		}
	}
	used := false
	for _, action := range definition.Actions {
		for _, requirement := range action.Credentials {
			if requirement.Name != slot {
				continue
			}
			if requirement.OAuth2 == nil {
				return false
			}
			used = true
		}
	}
	return used
}

func requirementsUseSlot(requirements []CredentialRequirement, slot string) bool {
	for _, requirement := range requirements {
		if requirement.Name == slot {
			return true
		}
	}
	return false
}
