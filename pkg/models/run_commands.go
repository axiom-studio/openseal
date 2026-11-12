package models

type RunCommandStore interface {
	PortfolioStore
	RunActivityStore
	ActionStore
}

type RunForkStore interface {
	PortfolioStore
	RunDependencyStore
	RunActivityStore
}
