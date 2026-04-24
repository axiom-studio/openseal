package persona

// Persona is a 1:1 extension of AgentInstance that gives it LLM reasoning capability.
// When an AgentInstance has a Persona, triggers go through LLM decision-making
// instead of directly executing a fixed workflow.
//
// The Persona.Id IS the AgentInstanceId - they share the same primary key.
type Persona struct {
	// Id is also the AgentInstanceId (1:1 relationship, shared PK)
	Id int `json:"id"`

	// Agent identity
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Avatar      string `json:"avatar"`

	// LLM configuration
	SystemPrompt     string   `json:"systemPrompt"`
	Tone             string   `json:"tone"` // professional, casual, technical
	ExpertiseTags    []string `json:"expertiseTags"`
	LLMProvider      string   `json:"llmProvider"` // openai, anthropic, gemini, ollama
	LLMModel         string   `json:"llmModel"`
	LLMTemperature   float64  `json:"llmTemperature"`
	LLMMaxTokens     int      `json:"llmMaxTokens"`
	LLMApiKey        string   `json:"llmApiKey"`         // or use vault
	LLMCredentialId  int      `json:"llmCredentialId"`   // Vault credential ID for LLM API

	// Behavior
	AutoApprove       bool `json:"autoApprove"`        // Skip INPUT_REQUIRED for trusted users
	MaxToolIterations int  `json:"maxToolIterations"` // Prevent infinite loops
	TimeoutSeconds    int  `json:"timeoutSeconds"`
	StreamingEnabled  bool `json:"streamingEnabled"`
	MemoryEnabled     bool `json:"memoryEnabled"` // Persist conversation context

	// Status
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"` // active, paused, error
}

// ============ Constants ============

const (
	// Persona tones
	ToneProfessional = "professional"
	ToneCasual       = "casual"
	ToneTechnical    = "technical"

	// Persona status
	PersonaStatusActive = "active"
	PersonaStatusPaused = "paused"
	PersonaStatusError  = "error"
)

// PersonaService manages agent personas.
// This interface defines only the methods needed by the runtime package.
type PersonaService interface {
	GetPersona(id int) (*Persona, error)
}