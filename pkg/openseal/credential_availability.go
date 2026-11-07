package openseal

// SetCredentialAvailabilityChecker installs the host's pre-dispatch
// credential check: before a credential lease is issued, action workers ask
// it whether every slot of the bound action can be resolved for the run's
// umbrella and acting person, and turn a missing sign-in into a
// connect_account card. Connect-account resolutions use it as their
// evidence. Call it before workers start.
func (e *Engine) SetCredentialAvailabilityChecker(c CredentialAvailabilityChecker) {
	if e == nil {
		return
	}
	e.credentialAvailability = c
	for _, pool := range e.actionPools {
		pool.SetCredentialAvailabilityChecker(c)
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.SetCredentialAvailabilityChecker(c)
	}
	if e.governedWorker != nil {
		e.governedWorker.SetCredentialAvailabilityChecker(c)
	}
}
