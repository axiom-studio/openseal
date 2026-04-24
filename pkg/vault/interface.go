package vault

import "context"

// VaultService manages encrypted credentials.
// This interface defines only the methods needed by the runtime package.
type VaultService interface {
	// ResolveCredential decrypts and returns all fields from a credential
	ResolveCredential(ctx context.Context, name string, projectId *int) (map[string]interface{}, error)

	// GetCredentialAsVirtualNodeOutputs returns decrypted credential fields for use as virtual node outputs
	GetCredentialAsVirtualNodeOutputs(ctx context.Context, credId int) (map[string]interface{}, error)

	// GetDefaultLLMCredentialForChat returns the LLM credential marked as default for agent chat
	GetDefaultLLMCredentialForChat(ctx context.Context) (*VaultCredentialBean, error)
}

// VaultCredentialBean represents a vault credential metadata (no secret data).
type VaultCredentialBean struct {
	Id             int    `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	CredentialType string `json:"credentialType"`
	Scope          string `json:"scope"`
	TeamId         *int   `json:"teamId,omitempty"`
	Version        int    `json:"version"`
}