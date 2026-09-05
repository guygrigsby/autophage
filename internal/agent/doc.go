// Package agent is the adapter over jess, agentcore and llm: the models per
// tier, the triage call, the budgeted attempt run and the runner that takes
// a started attempt from token to outcome through the sandbox. It is the
// only package that imports those three modules.
package agent
