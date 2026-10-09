# 9. Push-to-deploy via release webhook

Status: Accepted
Date: 2026-10-09

## Context

Every improvement to autophage lands on main and then waits for a human to `ssh` the deploy host, pull and `make install-systemd`. Twice now the daemon on trig ran a stale binary while the CLI and the repo had moved on — most recently `autophage watch` 404'd because the installed daemon predated the route. The manual step is also the only manual step left in the loop, and autophage merging its own pull requests makes it the bottleneck: the tool that develops itself cannot deploy itself.

The operator wants each push to become a versioned point release, and the deploy host to converge on it without a button. A poll on trig would work but is a clock watching for what GitHub already knows; GitHub can push instead.

## Decision

Two halves, event-driven end to end.

CI: every green push to main publishes a point release. The release job runs only after `make check` and `make test` pass, computes the next patch tag from the highest existing `v` tag, builds `autophaged` and `autophage` for linux/amd64 and darwin/arm64 with the tag stamped into `main.version`, and publishes them with a `checksums.txt` (release job in `ci.yml`).

Deploy: the repository's release webhook (event `release`, action `published`) targets the daemon's `POST /webhook/deploy` through Funnel, verified with its own HMAC secret (`AUTOPHAGE_DEPLOY_WEBHOOK_SECRET`, a different secret from the App's: this one is host code execution). The handler pins the repository (`deploy.repo` in config; the endpoint does not exist when it is empty), requires every asset URL to live under that repo's releases, verifies each binary against the release's sha256 checksum, and installs by rename into the install dir — atomic, the same trick `make install` uses. A redelivery of the running version is a no-op.

Restart semantics: the install never waits, the restart does. If attempts or repair rounds are running the restart is deferred; the schedulers' on-done hook cuts over at the first idle moment. The restart itself is a SIGTERM to the daemon's own process: the existing graceful path drains, and systemd's `Restart=always` brings the new binary up. A deploy therefore never aborts an attempt just to happen sooner.

Bootstrap is manual once per host: deploy a build that has the handler, set the secret and `deploy.repo`, add the Funnel path and create the repo webhook. After that the loop is closed.

## Consequences

- Push to green main is the whole deployment procedure; `git describe` on the host stops mattering because the running version is exactly a release tag.
- The deploy secret is the most sensitive value autophage handles: anyone holding it can ship a binary to the host. It is never written to the repo or shared with the App webhook secret, and the payload checks (pinned repo, pinned asset URL prefix) bound what a leaked secret can point at.
- A broken release bricks the pipeline at exactly one place: the next release still ships (CI is independent), and rollback is `gh release download` of the previous tag plus one rename.
- darwin/arm64 binaries ride along for local use; only linux/amd64 installs on trig.
- Releases from branches or failing builds do not exist; the webhook has nothing to see on a red push.
