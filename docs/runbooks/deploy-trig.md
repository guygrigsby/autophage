# Deploy autophage on trig

First live deployment: native Postgres, systemd `--user`, Tailscale Funnel and
a GitHub App. See ADR [0004](../adr/0004-github-app-webhooks-and-deployment.md)
for why.

Each step names who runs it. **operator**: needs a browser, a sudo password,
or is otherwise not safely scriptable. **ssh**: driven from the Mac over ssh
once trig has a working checkout.

Nothing past step 1 works until step 1 is done: trig clones autophage from
GitHub, and the build needs real tags for `jess` and `llm` instead of the
`replace` directives in `go.mod`.

## 1. Release prerequisites (operator)

Push and tag `llm` and `jess`, then drop the `replace` directives that point
at sibling checkouts in `gyr` and `autophage`.

```bash
cd <path to llm checkout>
git tag v0.4.0
git push --follow-tags
```

```bash
cd <path to jess checkout>
git tag v0.1.0
git push --follow-tags
```

```bash
cd <path to gyr checkout>
go mod edit -dropreplace=github.com/guygrigsby/jess
go get github.com/guygrigsby/jess@v0.1.0
go mod tidy
git add go.mod go.sum
git commit -m "drop jess replace directive"
git push
```

```bash
cd /Users/guygrigsby/projects/autophage
go mod edit -dropreplace=github.com/guygrigsby/jess -dropreplace=github.com/guygrigsby/llm
go get github.com/guygrigsby/jess@v0.1.0 github.com/guygrigsby/llm@v0.4.0
go mod tidy
git add go.mod go.sum
git commit -m "drop jess and llm replace directives"
git push
```

## 2. Postgres on trig (operator, sudo)

```bash
sudo dnf install -y postgresql-server postgresql-contrib
sudo postgresql-setup --initdb
sudo systemctl enable --now postgresql
sudo -u postgres createuser guygrigsby
sudo -u postgres createdb -O guygrigsby autophage
```

Peer auth over the Unix socket, no password. `db.url` in step 6's config
points at the socket directly: `postgres:///autophage?host=/var/run/postgresql`.

## 3. Checkout and sandbox image (ssh)

```bash
git clone git@github.com:guygrigsby/autophage.git ~/projects/autophage
cd ~/projects/autophage
make image
make image-test
```

`make image` also needs a `jess` sibling checkout next to `autophage/` on
trig, same as local builds (see the README's Building section), until the
Containerfile drops that `COPY jess /jess` line.

## 4. GitHub App (operator, browser)

Register at `github.com/settings/apps/new`, named `autophage`:

- Webhook URL: `https://trig.guy.ts.net/webhook/github`
- Webhook secret: generate one, do not reuse elsewhere
- Permissions: contents write, issues write, pull requests write, metadata read
- Subscribe to events: issues, installation, installation_repositories

Download the private key, then move it onto trig:

```bash
scp <downloaded App key>.pem trig:~/.config/autophage/app.pem
ssh trig chmod 0600 ~/.config/autophage/app.pem
```

Note the App id and the bot login (`autophage[bot]`) for step 6.

## 5. Secrets (ssh)

Write `~/.config/autophage/env` on trig, mode 0600, values from the op cache.
Never write the actual secret values into this runbook or any committed file.

```bash
ssh trig 'install -m 0600 /dev/null ~/.config/autophage/env'
ssh trig 'cat >> ~/.config/autophage/env' <<'EOF'
AUTOPHAGE_GITHUB_WEBHOOK_SECRET=<webhook secret>
OPENROUTER_API_KEY=<openrouter api key>
EOF
```

## 6. Config (ssh)

```bash
ssh trig 'cp ~/projects/autophage/config.example.toml ~/.config/autophage/config.toml'
```

Edit `~/.config/autophage/config.toml` on trig:

- `db.url = "postgres:///autophage?host=/var/run/postgresql"`
- `github.app_id` to the App id from step 4
- `github.operator_login = "guygrigsby"`
- `sandbox.concurrency = 2` (already the default; confirm it stayed)
- the three `[model.*]` ids under `model.triage`, `model.auto`, `model.approved`
  (already set from ADR 0006; override only if that changed)

## 7. Funnel (ssh)

```bash
ssh trig tailscale funnel --bg --set-path /webhook/github http://127.0.0.1:8080/webhook/github
```

If the tailnet ACL refuses Funnel on trig (**operator**, admin console):

```json
"nodeAttrs": [{"target": ["trig"], "attr": ["funnel"]}]
```

**Verify:**

```bash
curl -si https://trig.guy.ts.net/webhook/github -X POST
```

Expect `401`. That means Funnel reached the daemon and the daemon rejected
the unsigned request; anything else means Funnel or the daemon is not up yet.

## 8. Service (ssh)

```bash
ssh trig 'cd ~/projects/autophage && make install-systemd'
ssh trig loginctl enable-linger guygrigsby
```

**Verify:**

```bash
ssh trig systemctl --user status autophaged.service
```

Expect `active (running)`.

## 9. First live issue (operator, browser)

Install the App on one test repository. Open an issue as the owner with a
small typo fix.

**Verify:**

```bash
ssh trig 'cd ~/projects/autophage && ./autophage cases'
```

Expect the case to move Received, then Queued, then Attempting, then within
the `auto` budget either Done with a pull request or AwaitingApproval with a
comment.

```bash
ssh trig 'cd ~/projects/autophage && ./autophage why <attempt-id>'
```

Prints the jess ledger chain for that attempt.

## 10. Metrics (ssh, then operator on bee)

```bash
ssh trig tailscale serve --bg --set-path /metrics http://127.0.0.1:8080/metrics
```

Tailnet only, never through Funnel. Add `trig.guy.ts.net:8080/metrics` as a
scrape target in bee's Prometheus config (**operator**, edit on bee).

## Rollback

```bash
ssh trig systemctl --user stop autophaged
ssh trig tailscale funnel reset
```

The Postgres database can be dropped if a clean restart is needed:

```bash
ssh trig dropdb autophage
```
