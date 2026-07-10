// Package capability defines portable, dependency-free skill contracts.
package capability

import "time"

type RiskLevel string

const (
	RiskLevelRead        RiskLevel = "read"
	RiskLevelWrite       RiskLevel = "write"
	RiskLevelExternal    RiskLevel = "external"
	RiskLevelProduction  RiskLevel = "production"
	RiskLevelDestructive RiskLevel = "destructive"
)

type SideEffect string

const (
	SideEffectNone        SideEffect = "none"
	SideEffectRead        SideEffect = "read"
	SideEffectWrite       SideEffect = "write"
	SideEffectExternal    SideEffect = "external"
	SideEffectDestructive SideEffect = "destructive"
)

type IdempotencyMode string

const (
	IdempotencyNone      IdempotencyMode = "none"
	IdempotencySupported IdempotencyMode = "supported"
	IdempotencyRequired  IdempotencyMode = "required"
)

type CredentialRequirement struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Optional bool   `json:"optional,omitempty"`
}

// CredentialReference is an opaque binding identifier. Resolved values must
// never be placed in definitions, model catalogs, prompts, or activity events.
type CredentialReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type ActionRetryPolicy struct {
	MaxAttempts    int      `json:"maxAttempts"`
	InitialBackoff Duration `json:"initialBackoff,omitempty"`
	MaxBackoff     Duration `json:"maxBackoff,omitempty"`
}

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Action struct {
	Name                 string                  `json:"name"`
	Description          string                  `json:"description"`
	InputSchema          map[string]interface{}  `json:"inputSchema"`
	OutputSchema         map[string]interface{}  `json:"outputSchema,omitempty"`
	SideEffect           SideEffect              `json:"sideEffect"`
	Risk                 RiskLevel               `json:"risk"`
	Permissions          []string                `json:"permissions,omitempty"`
	Credentials          []CredentialRequirement `json:"credentials,omitempty"`
	Timeout              Duration                `json:"timeout,omitempty"`
	Retry                ActionRetryPolicy       `json:"retry"`
	Idempotency          IdempotencyMode         `json:"idempotency"`
	DryRunAction         string                  `json:"dryRunAction,omitempty"`
	CompensationAction   string                  `json:"compensationAction,omitempty"`
	EmittedArtifactTypes []string                `json:"emittedArtifactTypes,omitempty"`
	EmittedEventTypes    []string                `json:"emittedEventTypes,omitempty"`
	Transport            *TransportReference     `json:"transport,omitempty"`
}

type TransportReference struct {
	Kind      string                       `json:"kind"`
	Endpoint  string                       `json:"endpoint,omitempty"`
	Arguments map[string]TransportArgument `json:"arguments,omitempty"`
}

// TransportArgument deterministically projects a model-visible action input
// or a compiler-provided literal into the transport-specific call envelope.
// Credentials are deliberately resolved and passed out of band.
type TransportArgument struct {
	SourceArgument string      `json:"sourceArgument,omitempty"`
	Literal        interface{} `json:"literal,omitempty"`
}

type PromptModule struct {
	Instructions           string   `json:"instructions"`
	AlwaysActive           bool     `json:"alwaysActive,omitempty"`
	UserInvocable          bool     `json:"userInvocable"`
	DisableModelInvocation bool     `json:"disableModelInvocation,omitempty"`
	AllowedTools           []string `json:"allowedTools,omitempty"`
}

type Requirements struct {
	OperatingSystems []string `json:"operatingSystems,omitempty"`
	Executables      []string `json:"executables,omitempty"`
	AnyExecutables   []string `json:"anyExecutables,omitempty"`
	Environment      []string `json:"environment,omitempty"`
	Configuration    []string `json:"configuration,omitempty"`
	Compatibility    string   `json:"compatibility,omitempty"`
	AlwaysAvailable  bool     `json:"alwaysAvailable,omitempty"`
}

type Installer struct {
	ID               string   `json:"id,omitempty"`
	Kind             string   `json:"kind"`
	Label            string   `json:"label,omitempty"`
	OperatingSystems []string `json:"operatingSystems,omitempty"`
	Executables      []string `json:"executables,omitempty"`
	Package          string   `json:"package,omitempty"`
	Module           string   `json:"module,omitempty"`
	Formula          string   `json:"formula,omitempty"`
	URL              string   `json:"url,omitempty"`
	Archive          string   `json:"archive,omitempty"`
	Extract          *bool    `json:"extract,omitempty"`
	StripComponents  *int     `json:"stripComponents,omitempty"`
	TargetDirectory  string   `json:"targetDirectory,omitempty"`
}

type Resource struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

type SourceProvenance struct {
	Format          string                 `json:"format"`
	Registry        string                 `json:"registry,omitempty"`
	Publisher       string                 `json:"publisher,omitempty"`
	Reference       string                 `json:"reference,omitempty"`
	ResolvedVersion string                 `json:"resolvedVersion,omitempty"`
	Digest          string                 `json:"digest,omitempty"`
	License         string                 `json:"license,omitempty"`
	Homepage        string                 `json:"homepage,omitempty"`
	Trust           map[string]interface{} `json:"trust,omitempty"`
}

type Definition struct {
	ID               string             `json:"id"`
	Version          string             `json:"version"`
	Name             string             `json:"name"`
	Description      string             `json:"description,omitempty"`
	Icon             string             `json:"icon,omitempty"`
	ConfigurationKey string             `json:"configurationKey,omitempty"`
	Actions          map[string]Action  `json:"actions"`
	Transport        TransportReference `json:"transport"`
	Prompt           *PromptModule      `json:"prompt,omitempty"`
	Requirements     Requirements       `json:"requirements,omitempty"`
	Installers       []Installer        `json:"installers,omitempty"`
	Resources        []Resource         `json:"resources,omitempty"`
	Source           *SourceProvenance  `json:"source,omitempty"`
}

type ArgumentRule struct {
	Const interface{}   `json:"const,omitempty"`
	Enum  []interface{} `json:"enum,omitempty"`
}

type ScopeReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Binding struct {
	ID                   string                             `json:"id"`
	Scope                ScopeReference                     `json:"scope"`
	DeploymentID         string                             `json:"deploymentId"`
	SkillID              string                             `json:"skillId"`
	SkillVersion         string                             `json:"skillVersion"`
	Disabled             bool                               `json:"disabled,omitempty"`
	AllowedActions       []string                           `json:"allowedActions"`
	EnablePrompt         bool                               `json:"enablePrompt,omitempty"`
	MaximumRisk          RiskLevel                          `json:"maximumRisk"`
	ArgumentRestrictions map[string]map[string]ArgumentRule `json:"argumentRestrictions,omitempty"`
	Credentials          map[string]CredentialReference     `json:"credentials,omitempty"`
	Config               map[string]interface{}             `json:"config,omitempty"`
	Revision             int64                              `json:"revision"`
}

type ModelAction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	SkillID     string                 `json:"skillId"`
	Version     string                 `json:"version"`
	Action      string                 `json:"action"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Risk        RiskLevel              `json:"risk"`
	SideEffect  SideEffect             `json:"sideEffect"`
}

type ModelPrompt struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	SkillID        string `json:"skillId"`
	Version        string `json:"version"`
	AlwaysActive   bool   `json:"alwaysActive,omitempty"`
	UserInvocable  bool   `json:"userInvocable"`
	ModelInvocable bool   `json:"modelInvocable"`
}

type BoundAction struct {
	Definition *Definition
	Action     Action
	Binding    *Binding
}
