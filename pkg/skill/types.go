package skill

import (
	sdk_skill "github.com/axiom-studio/skills.sdk/skill"
)

type SkillManifest = sdk_skill.SkillManifest

type SkillSpec struct {
	Tools []ToolDefinition `json:"tools"`
	MCP   MCPConfig        `json:"mcp"`
}

type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type MCPConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []string `json:"env"`
}

type RepoConfig struct {
	Name          string `json:"name"`
	Url           string `json:"url"`
	Description   string `json:"description"`
	AuthType      string `json:"authType"`
	AccessToken   string `json:"accessToken,omitempty"`
	SshPrivateKey string `json:"sshPrivateKey,omitempty"`
	UserName      string `json:"userName,omitempty"`
	Password      string `json:"password,omitempty"`
	Branch        string `json:"branch,omitempty"`
}

type SkillInstallRequest struct {
	RepoURLs []string `json:"repoUrls"`
	SkillIDs []string `json:"skillIds,omitempty"`
}
