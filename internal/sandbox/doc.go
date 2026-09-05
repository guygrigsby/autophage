// Package sandbox prepares a case's workspace on host disk and runs the
// agent's tools inside a rootless podman container with no network and no
// secrets. It is the only package that runs git or podman.
package sandbox
