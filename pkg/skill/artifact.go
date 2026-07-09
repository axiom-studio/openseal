package skill

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
