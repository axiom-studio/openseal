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

// CredentialBindingChoice is a secret-safe, operator-facing option for one
// authorized opaque credential reference. Hosts resolve the reference out of
// band; neither the reference nor its display label is model catalog input.
type CredentialBindingChoice struct {
	Reference   CredentialReference `json:"reference"`
	DisplayName string              `json:"displayName"`
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
	// SemanticArguments maps portable roles such as "target", "body", or
	// "artifact" to exact input-schema property names. Product surfaces use
	// these roles to compose actions without guessing connector-specific fields.
	SemanticArguments map[string]string `json:"semanticArguments,omitempty"`
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
	Instructions           string                  `json:"instructions"`
	AlwaysActive           bool                    `json:"alwaysActive,omitempty"`
	UserInvocable          bool                    `json:"userInvocable"`
	DisableModelInvocation bool                    `json:"disableModelInvocation,omitempty"`
	AllowedTools           []string                `json:"allowedTools,omitempty"`
	Credentials            []CredentialRequirement `json:"credentials,omitempty"`
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

// Resource kinds are deliberately closed so a host can apply predictable
// permissions without guessing whether an imported artifact is executable.
const (
	ResourceKindFile      = "resource"
	ResourceKindScript    = "script"
	ResourceKindReference = "reference"
	ResourceKindAsset     = "asset"
)

type SourceProvenance struct {
	Identity        string                 `json:"identity,omitempty"`
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
	ID               string   `json:"id"`
	Version          string   `json:"version"`
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	Icon             string   `json:"icon,omitempty"`
	Category         string   `json:"category,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	ConfigurationKey string   `json:"configurationKey,omitempty"`
	// BindingConfigSchema defines non-secret, host-owned configuration that is
	// fixed when a Skill is bound. It is never part of model-visible action
	// input and is delivered to tool hosts separately from action arguments.
	BindingConfigSchema map[string]interface{} `json:"bindingConfigSchema,omitempty"`
	Actions             map[string]Action      `json:"actions"`
	Transport           TransportReference     `json:"transport"`
	Prompt              *PromptModule          `json:"prompt,omitempty"`
	Requirements        Requirements           `json:"requirements,omitempty"`
	Installers          []Installer            `json:"installers,omitempty"`
	Resources           []Resource             `json:"resources,omitempty"`
	Source              *SourceProvenance      `json:"source,omitempty"`
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
	SourceIdentity       string                             `json:"sourceIdentity,omitempty"`
	Disabled             bool                               `json:"disabled,omitempty"`
	AllowedActions       []string                           `json:"allowedActions"`
	EnablePrompt         bool                               `json:"enablePrompt,omitempty"`
	MaximumRisk          RiskLevel                          `json:"maximumRisk"`
	ArgumentRestrictions map[string]map[string]ArgumentRule `json:"argumentRestrictions,omitempty"`
	Credentials          map[string]CredentialReference     `json:"credentials,omitempty"`
	Config               map[string]interface{}             `json:"config,omitempty"`
	Revision             int64                              `json:"revision"`
	CreatedAt            time.Time                          `json:"createdAt,omitempty"`
	UpdatedAt            time.Time                          `json:"updatedAt,omitempty"`
	Lifecycle            []BindingLifecycleEntry            `json:"lifecycle,omitempty"`
}

type BindingLifecycleAction string

const (
	BindingLifecycleCreated  BindingLifecycleAction = "created"
	BindingLifecycleUpdated  BindingLifecycleAction = "updated"
	BindingLifecycleEnabled  BindingLifecycleAction = "enabled"
	BindingLifecycleDisabled BindingLifecycleAction = "disabled"
)

// BindingActor identifies the principal responsible for a management change.
// Hosts authenticate and authorize this principal before invoking the portable
// contract; OpenSeal persists only the product-neutral attribution.
type BindingActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type BindingLifecycleEntry struct {
	Revision int64                  `json:"revision"`
	Action   BindingLifecycleAction `json:"action"`
	Actor    BindingActor           `json:"actor"`
	Reason   string                 `json:"reason"`
	At       time.Time              `json:"at"`
}

type ModelAction struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	// DeploymentID identifies the Agent or Team binding owner. It is supplied
	// by the kernel, never by the model, and prevents a mixed Team turn from
	// resolving an Agent action against Team credentials (or vice versa).
	DeploymentID      string                 `json:"deploymentId,omitempty"`
	SkillID           string                 `json:"skillId"`
	Version           string                 `json:"version"`
	Action            string                 `json:"action"`
	InputSchema       map[string]interface{} `json:"inputSchema"`
	SemanticArguments map[string]string      `json:"semanticArguments,omitempty"`
	Risk              RiskLevel              `json:"risk"`
	SideEffect        SideEffect             `json:"sideEffect"`
}

// BindingReference selects one exact, versioned binding. Persisting this
// reference on an ActionCall prevents execution from drifting to another
// account, credential set, configuration, or policy revision.
type BindingReference struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type ModelPrompt struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	SkillID         string `json:"skillId"`
	Version         string `json:"version"`
	AlwaysActive    bool   `json:"alwaysActive,omitempty"`
	UserInvocable   bool   `json:"userInvocable"`
	ModelInvocable  bool   `json:"modelInvocable"`
}

type BoundAction struct {
	Definition *Definition
	Action     Action
	Binding    *Binding
}
