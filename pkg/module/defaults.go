package module

func NewDefaultRegistry() (*Registry, error) {
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
