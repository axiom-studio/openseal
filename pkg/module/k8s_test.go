package module

import "testing"

func TestProcessExecutorRequiresAnExplicitRunnerImageAndValidEnablement(t *testing.T) {
	t.Setenv("OPENSEAL_BREW_RUNNER_IMAGE", "")
	t.Setenv("OPENSEAL_PROCESS_EXECUTOR_ENABLED", "")
	if ProcessExecutorEnabled() {
		t.Fatal("process executor enabled without an explicit runner image")
	}
	t.Setenv("OPENSEAL_BREW_RUNNER_IMAGE", " registry.example/openseal-brew@sha256:abc ")
	if !ProcessExecutorEnabled() || GetBrewProcessRunnerImage() != "registry.example/openseal-brew@sha256:abc" {
		t.Fatal("explicit process runner was not enabled")
	}
	t.Setenv("OPENSEAL_PROCESS_EXECUTOR_ENABLED", "not-a-bool")
	if ProcessExecutorEnabled() {
		t.Fatal("invalid enablement did not fail closed")
	}
	t.Setenv("OPENSEAL_PROCESS_EXECUTOR_ENABLED", "false")
	if ProcessExecutorEnabled() {
		t.Fatal("disabled process executor remained enabled")
	}
}
