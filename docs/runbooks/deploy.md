# Deploy autophage on a Linux host

First live deployment: native Postgres, systemd `--user`, Tailscale Funnel and
a GitHub App. See ADR [0004](../adr/0004-github-app-webhooks-and-deployment.md)
for why.

Each step names who runs it. **operator**: needs a browser, a sudo password,
or is otherwise not safely scriptable. **ssh**: driven from the Mac over ssh
once the deploy host has a working checkout.

Nothing that clones from GitHub works until step 1 is done: the deploy host clones
autophage (and jess) from GitHub, and the build needs real tags for `jess`
and `llm` instead of the `replace` directives in `go.mod`. Step 2 (Postgres)
does not depend on step 1 and can run first or in parallel.

## 1. Release prerequisites (operator)

Push and tag `llm` and `jess`, then drop the `replace` directives that point
at sibling checkouts in `gyr` and `autophage`.

```bash
cd <path to llm checkout>
git tag -a v0.4.0 -m "openrouter adapter"
git push
git push origin v0.4.0
```

llm already has a local annotated `v0.4.0` tag. If `git tag -a` fails with
"tag already exists", that is fine, skip straight to the two pushes above.

```bash
cd <path to jess checkout>
git tag -a v0.1.0 -m "mcp adapter, ReleaseAgent"
git push
git push origin v0.1.0
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
cd ~/projects/autophage
go mod edit -dropreplace=github.com/guygrigsby/jess -dropreplace=github.com/guygrigsby/llm
go get github.com/guygrigsby/jess@v0.1.0 github.com/guygrigsby/llm@v0.4.0
go mod tidy
git add go.mod go.sum
git commit -m "drop jess and llm replace directives"
git push
```

## 2. Postgres on the deploy host (operator, sudo)

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
git clone git@github.com:guygrigsby/jess.git ~/projects/jess
git -C ~/projects/jess checkout v0.1.0
cd ~/projects/autophage
make image
make image-test
```

## 4. GitHub App (operator, browser)

Register at `github.com/settings/apps/new`, named `autophage`:

- Webhook URL: `https://<host>.<tailnet>.ts.net/webhook/github`
- Webhook secret: generate one, do not reuse elsewhere
- Permissions: contents write, issues write, pull requests write, metadata read
- Subscribe to events: issues, installation, installation_repositories

Download the private key, then move it onto <host>:

```bash
scp <downloaded App key.pem> <host>:~/.config/autophage/app.pem
ssh <host> chmod 0600 ~/.config/autophage/app.pem
```

Note the App id and the bot login (`autophage[bot]`) for step 6.

**Verify:** the App's "Permissions & events" page must list Issues under
subscribed events, or GitHub never delivers an issue (the installation
delivery still succeeds, which makes the gap easy to miss). Before opening the
first issue, the App's recent deliveries page should show `installation`
with 202 and nothing failed.

## 5. Secrets (ssh)

Write `~/.config/autophage/env` on the deploy host, mode 0600, values from your secret store.
Never write the actual secret values into this runbook or any committed file.

```bash
ssh <host> 'install -m 0600 /dev/null ~/.config/autophage/env'
ssh <host> 'cat >> ~/.config/autophage/env' <<'EOF'
AUTOPHAGE_GITHUB_WEBHOOK_SECRET=<webhook secret>
OPENROUTER_API_KEY=<openrouter api key>
EOF
```

## 6. Config (ssh)

```bash
ssh <host> 'cp ~/projects/autophage/config.example.toml ~/.config/autophage/config.toml'
```

Edit `~/.config/autophage/config.toml` on <host>:

- `db.url = "postgres:///autophage?host=/var/run/postgresql"`
- `github.app_id` to the App id from step 4
- `github.operator_login = "guygrigsby"`
- `sandbox.concurrency = 2` (already the default; confirm it stayed)
- the three `[model.*]` ids under `model.triage`, `model.auto`, `model.approved`
  (already set from ADR 0006; override only if that changed)

## 7. Funnel (ssh)

The port in every proxy target below is the daemon's `listen` port from
`config.toml`. The example config says 8080; change both if something else
on the host already holds it (this runbook was first run on a host where it
did).

```bash
ssh <host> tailscale funnel --bg --set-path /webhook/github http://127.0.0.1:8080/webhook/github
```

If the tailnet ACL refuses Funnel on the deploy host (**operator**, admin console):

```json
"nodeAttrs": [{"target": ["<host>"], "attr": ["funnel"]}]
```

**Verify:**

```bash
curl -si -X POST -H 'Content-Type: application/json' -d '{}' https://<host>.<tailnet>.ts.net/webhook/github
```

Expect `401` (`unauthenticated`): Funnel reached the daemon and the daemon
rejected the unsigned JSON request. A bare POST without the JSON content type
gets `400` instead, which also proves the daemon answered. Anything else means
Funnel or the daemon is not up yet.

### Names and what is public

Funnel is per node and per port: everything served on the node's 443 is
public once Funnel is on, so put nothing but the webhook path there. Do not
serve `/metrics` from the node's 443 (a first deployment did, and it was on
the internet until removed).

A tailnet name for the daemon (`https://autophage.<tailnet>.ts.net` for the
CLI, metrics and dashboards) is a Tailscale Service, not a machine: define
it wherever the tailnet policy is managed as code (a `svc:` entry pointing at
the daemon's `listen` port, granted to admins) and let that sync tool set the
serve handlers on the host. Funnel cannot publish a service name on the
current CLI, so the App's webhook URL stays on the host node's name; the
service name carries everything tailnet-only.

## 8. Service (ssh)

```bash
ssh <host> 'cd ~/projects/autophage && make install-systemd'
ssh <host> loginctl enable-linger $USER
```

**Verify:**

```bash
ssh <host> systemctl --user status autophaged.service
```

Expect `active (running)`.

## 9. First live issue (operator, browser)

Install the App on one test repository. Open an issue as the owner with a
small typo fix.

**Verify:**

```bash
ssh <host> 'cd ~/projects/autophage && ./autophage auth login'
```

The mint endpoint only accepts loopback requests, so login has to run from a
shell on the deploy host itself, over this ssh session, not from your workstation's CLI (the CLI reads the daemon's `listen` port from the same config file) and
not through Funnel.

```bash
ssh <host> 'cd ~/projects/autophage && ./autophage cases'
```

Expect the case to move Received, then Queued, then Attempting, then within
the `auto` budget either Done with a pull request or AwaitingApproval with a
comment.

```bash
ssh <host> 'cd ~/projects/autophage && ./autophage why <attempt-id>'
```

Prints the jess ledger chain for that attempt.

## 10. Metrics (operator on the Prometheus host)

`/metrics` is served by the tailnet service name, never from the node's
Funnel port (see step 7). Add `https://autophage.<tailnet>.ts.net/metrics` as
a scrape target in the Prometheus host's scrape config (**operator**). The
service terminates TLS with a ts.net certificate on 443, so the daemon's
own port is never reachable from the Prometheus host.

## Rollback

```bash
ssh <host> systemctl --user stop autophaged.service
ssh <host> tailscale funnel reset
```

The Postgres database can be dropped if a clean restart is needed:

```bash
ssh <host> dropdb autophage
```
