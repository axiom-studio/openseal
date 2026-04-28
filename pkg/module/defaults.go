package module

// RegistryConfig is deprecated - use executor.Registry for code execution
type RegistryConfig struct {
	Namespace   string
	PythonImage string
}

func NewDefaultRegistry() (*Registry, error) {
	return NewDefaultRegistryWithConfig(nil)
}

// NewDefaultRegistryWithConfig creates a registry with default step executors
// Note: Code execution is now handled by executor.Registry, not module.Registry
func NewDefaultRegistryWithConfig(config *RegistryConfig) (*Registry, error) {
	r := NewRegistry()

	executors := []StepExecutor{
		NewFuncExecutor("if", ifExecute),
		NewFuncExecutor("switch", switchExecute),
		NewFuncExecutor("transform", transformExecute),
		NewFuncExecutor("set", setExecute),
		NewFuncExecutor("merge", mergeExecute),
		NewFuncExecutor("delay", delayExecute),
		NewFuncExecutor("http", httpExecute),
	}

	for _, exec := range executors {
		if err := r.Register(exec); err != nil {
			return nil, err
		}
	}

	return r, nil
}
