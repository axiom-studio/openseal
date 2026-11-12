// Package skillbinding holds how an agent's skill bindings are set up,
// connected (apps, accounts, credentials), upgraded and maintained: the
// management Skill's actions and their validator and dispatcher, setup,
// credential and connect-app requests, skill reference upgrades and the
// skill authority of Team runs.
//
// It sits on the agent run contract (agentrun) and pkg/models. Action
// execution, conversations and the run loop build on it.
package skillbinding
