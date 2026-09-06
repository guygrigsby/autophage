# 4. GitHub App with webhooks, Funnel ingress, native Postgres on the deploy host

Status: Accepted
Date: 2026-09-04

## Context

The watcher must learn about issues as they happen (event-driven, not a clock), across every enrolled repository, with credentials scoped narrowly. Options: a GitHub App with webhooks, a polling daemon on a personal token, or a workflow file in every repository.

Where it runs: one Linux host with spare cores and memory, podman 5.8 and a Tailscale node; the alternative host had no headroom and binds its proxy to the tailnet only.

## Decision

- A GitHub App named autophage. Installation selects the repositories; that is the enrollment. Permissions: contents write, issues write, pull requests write, metadata read. Events: `issues` (opened, labeled, closed), `installation`, `installation_repositories`.
- Installation tokens minted per attempt, scoped to the one repository, one hour.
- Deliveries are stored byte-exact with the delivery id as the idempotency key and acknowledged before processing. Processing is a separate fact so redelivery and crash recovery replay safely.
- The daemon and the sandboxes run on the deploy host. Ingress is Tailscale Funnel on the deploy host for the single webhook path.
- Postgres 17 installed natively on the deploy host (`dnf install postgresql-server`), one database for autophage's tables and jess's ledger tables. No container for the database.
- `autophaged` runs under systemd `--user` with linger. rookery ships launchd only; a systemd unit template is added.

## Consequences

- Adding a repository is a checkbox on the installation. No per-repository workflow files, no personal token with blanket scope.
- Funnel depends on the tailnet ACL permitting it for the deploy host. Verify first; Cloudflare Tunnel is the fallback.
- One box holds the daemon, the database, the workspaces and the containers. Losing that host loses in-flight attempts; cases and deliveries survive in Postgres and re-run on boot.

## Alternatives

- Polling with a personal token: no ingress needed but clock-driven and all-or-nothing scope.
- GitHub Actions per repository: no infrastructure but a workflow and a secret in every repository, and no local caches.
- Postgres in a podman quadlet: rejected in favor of the native package on the box.
