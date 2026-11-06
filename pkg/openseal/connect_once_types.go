package openseal

import (
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// SkillPersonCredentialReferenceID is the reference ID of a per_person
// binding slot: the acting person's own sign-in fills it at run time.
const SkillPersonCredentialReferenceID = skill.PersonCredentialReferenceID

// IsSkillPersonCredentialReference reports whether ref is a per_person slot.
func IsSkillPersonCredentialReference(ref SkillCredentialReference) bool {
	return skill.IsPersonCredentialReference(ref)
}

// SkillBindingUsesPersonCredentials reports whether any slot of b is
// per_person.
func SkillBindingUsesPersonCredentials(b *SkillBinding) bool {
	return skill.BindingUsesPersonCredentials(b)
}

// SkillPersonCredentialAllowed reports whether slot of b may hold the person
// reference: only action credentials with OAuth2 qualify, and only on a
// binding that enables no conversation or callback adapter.
func SkillPersonCredentialAllowed(definition *SkillDefinition, b *SkillBinding, slot string) bool {
	return skill.PersonCredentialAllowed(definition, b, slot)
}

// MessageActingUser returns the human a conversation message acts for, or ""
// when no person sent it.
func MessageActingUser(conversation *Conversation, message *ChannelMessage) string {
	return runtime.MessageActingUser(conversation, message)
}

type (
	CredentialAvailabilityChecker = runtime.CredentialAvailabilityChecker
	CredentialAvailabilityRequest = runtime.CredentialAvailabilityRequest
	CredentialAvailability        = runtime.CredentialAvailability
	CredentialGap                 = runtime.CredentialGap
	ConnectAccountSlot            = runtime.ConnectAccountSlot
)

const (
	CredentialGapPersonSignIn    = runtime.CredentialGapPersonSignIn
	CredentialGapSharedSignIn    = runtime.CredentialGapSharedSignIn
	CredentialGapNoPerson        = runtime.CredentialGapNoPerson
	SkillSetupKindConnectAccount = runtime.SkillSetupKindConnectAccount
	ConnectAccountWakeType       = runtime.ConnectAccountWakeType
)
