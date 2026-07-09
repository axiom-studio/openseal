package skillmd

type ParsedSkill struct {
	Name            string
	Description     string
	License         string
	Compatibility   string
	AllowedTools    []string
	Version         string
	Homepage        string
	Invocation      InvocationPolicy
	CommandDispatch *CommandDispatch
	Metadata        SkillMetadata
	Frontmatter     map[string]interface{}
	Body            string
	RawContent      []byte
	SizeBytes       int
	Warnings        []string
}

type InvocationPolicy struct {
	UserInvocable          bool
	DisableModelInvocation bool
}

type CommandDispatch struct {
	Kind     string
	ToolName string
	ArgMode  string
}

type SkillMetadata struct {
	RequiresEnv    []string
	RequiresBins   []string
	RequiresConfig []string
	RequiresAnyBin []string
	PrimaryEnv     string
	SkillKey       string
	Emoji          string
	Homepage       string
	OS             []string
	Always         bool
	Install        []InstallSpec
	Raw            map[string]interface{}
	OpenClaw       *OpenClawMetadata
}

type OpenClawMetadata struct {
	Always     bool
	SkillKey   string
	PrimaryEnv string
	Emoji      string
	Homepage   string
	OS         []string
	Requires   RequiresConfig
	Install    []InstallSpec
}

type RequiresConfig struct {
	Env     []string
	Bins    []string
	AnyBins []string
	Config  []string
}

type InstallSpec struct {
	ID              string
	Kind            string
	Formula         string
	Package         string
	Module          string
	URL             string
	Archive         string
	Extract         *bool
	StripComponents *int
	TargetDir       string
	Bins            []string
	Label           string
	OS              []string
}

type ValidationError struct {
	Field      string
	Message    string
	LineNumber int
}
