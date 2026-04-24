package skillmd

type ParsedSkill struct {
	Name        string
	Description string
	Version     string
	Metadata    SkillMetadata
	Body        string
	RawContent  []byte
	SizeBytes   int
	Warnings    []string
}

type SkillMetadata struct {
	RequiresEnv    []string
	RequiresBins   []string
	RequiresConfig []string
	RequiresAnyBin []string
	PrimaryEnv     string
	Emoji          string
	Homepage       string
	OS             []string
	Always         bool
	Install        []InstallSpec
	OpenClaw       *OpenClawMetadata
}

type OpenClawMetadata struct {
	Requires RequiresConfig
}

type RequiresConfig struct {
	Env     []string
	Bins    []string
	AnyBins []string
	Config  []string
}

type InstallSpec struct {
	Kind    string
	Formula string
	Package string
	Bins    []string
	Label   string
	OS      []string
}

type ValidationError struct {
	Field      string
	Message    string
	LineNumber int
}
