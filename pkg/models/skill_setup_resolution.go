package models

// SkillSetupResolutionReceipt is kernel-authored evidence of the separately
// authorized setup interaction. Configuration saved is not provider access
// verified, and this receipt never grants action or credential authority.
type SkillSetupResolutionReceipt struct {
	Scope                   Scope  `json:"scope"`
	RunID                   string `json:"runId"`
	RequestID               string `json:"requestId"`
	Status                  string `json:"status"`
	ActionCallID            string `json:"actionCallId"`
	Revision                int64  `json:"revision"`
	ResolvedBindingID       string `json:"resolvedBindingId,omitempty"`
	ResolvedBindingRevision int64  `json:"resolvedBindingRevision,omitempty"`
}
