# app

A two-binary daemon/CLI service built on
[perch](https://github.com/guygrigsby/perch).

- `appd` — the daemon (serves the API + an optional embedded Svelte SPA).
- the CLI client (`auth login`, `whoami`).

## Quick start

```bash
make build
./appd &                 # starts on :8080
./app auth login         # mint + store a token
./app whoami             # authenticated call
```

## Make targets

`make help` lists everything. The important ones: `build`, `test`, `check`
(the quality gate), `dev` (hot-reload loop), and the launchd set
(`install-launchd`, `redeploy`, `service-restart`).
