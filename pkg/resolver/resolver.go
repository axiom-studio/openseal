package resolver

// Resolver defines the interface for resolving variable bindings.
// Implementations can resolve placeholders like {{.env.VAR}} or {{.secret.NAME}}
// from various sources (environment variables, secrets, configs, etc.)
type Resolver interface {
	Resolve(binding string, context map[string]any) (string, error)
}
