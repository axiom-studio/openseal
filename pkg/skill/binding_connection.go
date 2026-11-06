package skill

// Skills whose bindings always stand for a connection. pkg/mcp and the
// runtime own these IDs; they are repeated here because skill cannot import
// either.
const (
	mcpConnectionSkillID        = "openseal.mcp"
	externalConversationSkillID = "openseal.external-conversation"
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
		len(b.EnabledCallbackAdapters) > 0 || b.SkillID == mcpConnectionSkillID || b.SkillID == externalConversationSkillID
}
