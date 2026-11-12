package runtime

// SkillActionRequestConnection asks the user, through a durable in-chat
// card, to connect one catalog app (or an MCP server address). It pauses the
// requesting Run until the connection is active or the card is dismissed.
const SkillActionRequestConnection = "request_connection"

// SkillActionRequestCredential asks the user, through a durable in-chat card,
// to save a credential in the host's vault. It is not Skill setup: nothing
// about a binding changes. The request never carries a secret value.
const SkillActionRequestCredential = "request_credential"

const SkillActionRequestSetup = "request_setup"
