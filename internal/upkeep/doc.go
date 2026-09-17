// Package upkeep is autophage's second domain: a Bump is one dependabot
// pull request on a watched repository that autophage shepherds until its
// checks are green or it gives up. The aggregate enforces every invariant
// and refuses every transition its table does not list.
//
// Upkeep is not Resolution with a different trigger. Requester and Approval
// do not exist here, Trust is a constant rather than a verdict, and success
// is green checks rather than a pull request opened. What the two contexts
// share is a kernel of budgeted-execution types imported from resolution:
// Budget, Limit, Usage, FailureClass, Run and Brief. AbortReason is
// deliberately not shared; see RepairAbortReason.
//
// Nothing here imports outside the standard library and that kernel. GitHub,
// the sandbox and the agent are ports.
package upkeep
