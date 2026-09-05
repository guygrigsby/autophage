# autophage

A two-binary daemon/CLI service built on
[perch](https://github.com/guygrigsby/perch).

- `autophaged` — the daemon (serves the API + an optional embedded Svelte SPA).
- the CLI client (`auth login`, `whoami`).

## Quick start

```bash
make build
./autophaged &                 # starts on :8080
./autophage auth login         # mint + store a token
./autophage whoami             # authenticated call
```

## Make targets

`make help` lists everything. The important ones: `build`, `test`, `check`
(the quality gate), `dev` (hot-reload loop), and the launchd set
(`install-launchd`, `redeploy`, `service-restart`).
