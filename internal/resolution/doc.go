// Package resolution is autophage's domain: a Case is one issue in one
// enrolled repository that autophage is handling, from receipt through
// triage, approval and budgeted attempts to a pull request, a request for
// approval or a failure. The aggregate enforces every invariant and refuses
// every transition its table does not list. Nothing here imports outside the
// standard library; GitHub, the sandbox and the agent are ports.
package resolution
