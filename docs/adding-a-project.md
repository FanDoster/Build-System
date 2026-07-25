# Adding a project to the build system

How to put a repository onto the Build-System server so that pushing to it produces a
Docker image, and (optionally) a running container.

Audience: whoever — human or agent — is wiring up a new project. It assumes you can
`ssh` to the Hermes Linode and reach `https://fandoster.com/builds/`. For working on the
build server's own code, read [CLAUDE.md](../CLAUDE.md) instead.

---

## What it actually does

One project = one repository + branch → one image. When a build is triggered the runner
executes these steps, streaming everything to a live log:

| Step | Command (roughly) | Runs when |
| --- | --- | --- |
| `clone` | `git clone --depth 1 --branch <branch> <repo_url> <tmpdir>` | always |
| `checkout` | `git checkout <commit_sha>` | only when the trigger recorded a real SHA |
| `build` | `docker build -t registry.fandoster.com/<image_name>:latest -f <dockerfile_path> <tmpdir>` | always |
| `push` | `docker push registry.fandoster.com/<image_name>:latest` | always |
| `deploy` | `docker compose -f <compose> up -d --pull always --wait --wait-timeout 120 <service>` | only when **both** deploy fields are set |

The whole build is bounded by `BUILDS_BUILD_TIMEOUT` (default **30m**), and there is
exactly **one worker** — concurrent triggers queue rather than run in parallel.

The image tag is always `registry.fandoster.com/<image_name>:latest`. There are no
per-commit tags, so there is no rollback target; redeploying an older build means
rebuilding the older commit.

---

## Before you start

Decide two things:

1. **`image_name`** — this is the contract with everything downstream. Your compose
   file and Watchtower both reference `registry.fandoster.com/<image_name>:latest`.
   Renaming it later means editing the compose file on the server too. It does *not*
   have to match the project name (the build server's own project is named
   `Build-System` but pushes `builds`).
2. **How it gets deployed** — see [Step 3](#step-3-decide-how-it-gets-deployed). The
   options are "not at all", "Watchtower notices the new image", or "the build deploys
   it explicitly". Pick one; two of them together means two container recreates.

Your repo needs a **Dockerfile that builds from the repo root as context**, with no
build args and no secrets required at build time. Nothing else.

---

## Making authenticated API calls

The API sits behind a password. Two ways in:

**As a human:** sign in at `https://fandoster.com/builds/` and use the web UI. The
session cookie carries the auth; the UI sends the CSRF header for you.

**As a script or agent:** let the container expand its own password so you never handle
the secret. This is the standard pattern — use it for every example below:

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker exec builds sh -c "curl -s -H \"Authorization: Bearer \$BUILDS_PASSWORD\" -H \"X-Builds-Csrf: 1\" http://127.0.0.1:8080/api/projects"'
```

Two rules that will otherwise cost you an afternoon:

- **Every state-changing endpoint requires `X-Builds-Csrf: 1`.** Without it you get a
  403 that says exactly that.
- **When curling the container directly, drop the `/builds` prefix.** The proxy strips
  it, so inside it's `/api/projects`, not `/builds/api/projects`. Through the public
  URL, keep the prefix.

`/api/health` needs no auth and is the quickest liveness check.

---

## Step 1: create the project

Easiest by hand: the web UI, then fill in the rest under **Settings**.

From a script, write the payload to a file rather than fighting three levels of shell
quoting — this form is verified working:

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'cat > /tmp/new-project.json <<JSON
{"name":"my-app","repo_url":"https://github.com/FanDoster/my-app","branch":"main","image_name":"my-app","dockerfile_path":"Dockerfile"}
JSON
docker cp /tmp/new-project.json builds:/tmp/p.json >/dev/null
docker exec builds sh -c "curl -s -X POST -H \"Authorization: Bearer \$BUILDS_PASSWORD\" -H \"X-Builds-Csrf: 1\" -H \"Content-Type: application/json\" --data @/tmp/p.json http://127.0.0.1:8080/api/projects"
docker exec builds rm -f /tmp/p.json; rm -f /tmp/new-project.json'
```

Only `name`, `repo_url` and `image_name` are required. The response echoes the created
project including its `id`, which you need for everything else. A duplicate name gives
`409`; a bad payload gives `400` naming the missing fields.

Updating later uses the same shape with `PUT /api/projects/{id}` — omitted fields keep
their current value, so you can send just the one you're changing.

### Field reference

| Field | Default | Notes |
| --- | --- | --- |
| `name` | — | Required. Display name, must be unique (409 otherwise). |
| `repo_url` | — | Required. **Use the `https://` form** — see the webhook gotcha below. |
| `image_name` | — | Required. Pushed as `registry.fandoster.com/<image_name>:latest`. |
| `branch` | `main` | The branch that is cloned and watched. |
| `dockerfile_path` | `Dockerfile` | Relative to the repo root. Cannot escape the checkout. |
| `deploy_compose_path` | `""` | Absolute = server-managed file; relative = in-repo. Empty disables deploy. |
| `deploy_service_name` | `""` | The service in that compose file to bring up. Both fields must be set. |
| `no_cache` | `false` | Passes `--no-cache` to `docker build`. Turn on if you hit stale layers. |
| `webhook_secret` | `""` | HMAC secret for GitHub push events. Write-only in the UI. |
| `clone_token` | `""` | PAT for private repos. Write-only, scrubbed from logs. |
| `poll_enabled` | `false` | Git polling as an alternative to webhooks. |
| `poll_interval_secs` | `60` | Floored at **30**; anything lower is rejected with a 400. |

Secrets are write-only: `GET`/list responses never return `webhook_secret` or
`clone_token`, and the settings page only tells you whether one is set. To clear one,
send an explicit empty string (the UI has a "clear" checkbox); omitting the field keeps
the current value.

---

## Step 2: choose a trigger

### Manual

A button on the project page, or:

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker exec builds sh -c "curl -s -X POST -H \"Authorization: Bearer \$BUILDS_PASSWORD\" -H \"X-Builds-Csrf: 1\" http://127.0.0.1:8080/api/projects/<ID>/build"'
```

A manual build records the commit SHA as the literal string `manual`, which means the
checkout step is skipped and you get whatever the branch tip is at clone time.

### GitHub webhook (push-based, recommended)

In the GitHub repo: **Settings → Webhooks → Add webhook**

- **Payload URL:** `https://fandoster.com/builds/api/webhook/github`
- **Content type:** `application/json`
- **Secret:** generate one and put the same value in the project's `webhook_secret`
- **Events:** "Just the push event"

The endpoint is deliberately unauthenticated — GitHub cannot log in — so the secret is
what protects it. Without a secret set on the project, any caller who knows the URL can
trigger builds for it.

Matching is by **normalized repo URL + branch**: the handler strips `https://`/`http://`
and a trailing `.git` from both sides and compares. Consequences:

- `repo_url` must be the **HTTPS form**. `git@github.com:FanDoster/my-app.git` will
  never match the `clone_url` GitHub sends, and your pushes will silently no-op with
  `{"status":"ignored","reason":"no matching project"}`.
- Several projects may match one webhook (same repo, different branches) — each gets
  its own build.
- Payloads with no `head_commit` (branch deletions, some tag pushes) are ignored.

### Git polling (pull-based)

Enable per project under **Settings → Triggers**, or `"poll_enabled": true`. The server
runs `git ls-remote` every `poll_interval_secs` and queues a build when the branch tip
moves. Use it when the repo can't reach the server (no inbound webhook), or as a belt
alongside webhooks — they deduplicate, see below.

Behaviours worth knowing, all of them deliberate:

- **Enabling only seeds the baseline; it does not build.** Otherwise switching it on
  would immediately rebuild whatever the branch already pointed at.
- **A commit that already has a build is never built again** — any status, any trigger.
  This is what stops webhook + polling double-building the same push, and it also means
  a failed commit is not retried every sweep. Re-run it by hand if you want another go.
- **A failed probe keeps the last known tip**, so a remote coming back after an outage
  doesn't build a stale commit.
- **A commit landing mid-build is deferred, not dropped** — it builds on the next sweep
  once the queue clears.
- **Changing `repo_url` or `branch` resets the baseline**, so the next sweep re-seeds
  rather than building "not the old commit".

---

## Step 3: decide how it gets deployed

### Option A — build and push only

Leave both deploy fields empty. The image lands in the registry and nothing else
happens. Fine for libraries, base images, or anything you deploy by hand.

### Option B — Watchtower notices the new image

Add the label to the service in its compose file:

```yaml
labels:
  - "com.centurylinklabs.watchtower.enable=true"
```

Watchtower polls every **60s** (`WATCHTOWER_POLL_INTERVAL=60`) and recreates the
container when `:latest` changes. Simple, but the recreate happens on Watchtower's
schedule, not the build's — the build reports success before the deploy has happened,
and a failed deploy is invisible to the build log.

### Option C — the build deploys it explicitly (recommended for web apps)

Set both `deploy_compose_path` and `deploy_service_name`. The runner then runs:

```
docker compose -f <compose_path> up -d --pull always --wait --wait-timeout 120 <service>
```

`--wait` is why this is the better option: the build **fails visibly** if the new
container doesn't come up. A service with a `healthcheck` must actually become healthy;
one without only has to reach "running".

**Where the compose file goes.** The convention on this box is one directory per app:

```
/opt/docker/<app>/docker-compose.yml
```

so `deploy_compose_path` is `/opt/docker/my-app/docker-compose.yml`. That whole tree is
mounted **read-only** into the builds container, which is how the runner can read it.
An absolute path is used as-is; a relative path is resolved inside the cloned repo
instead (so a project can ship its own compose file), and is rejected if it escapes the
checkout.

Two subtleties, because compose runs *inside* the builds container against the *host's*
Docker daemon (via the mounted socket):

- **Bind-mount sources in your compose file are host paths**, resolved by the daemon.
- **The compose project name comes from the parent directory name**, which is why the
  file must live at `/opt/docker/<app>/` — put it somewhere else and compose will
  consider the existing containers to belong to a different project and leave them be.

A minimal app compose file, matching the house style:

```yaml
services:
  my-app:
    image: registry.fandoster.com/my-app:latest
    container_name: my-app
    restart: unless-stopped
    environment:
      LISTEN_ADDR: ":3000"
    ports:
      - "127.0.0.1:8085:3000"     # host port must be free — see below
    volumes:
      - my-app-data:/data

volumes:
  my-app-data:
```

Bind only to `127.0.0.1`; nginx is the only thing that should face the internet.
**Host ports already in use:** 3000, 5000, 8000, 8080, 8081, 8082, 8083, 8084. Check
before you pick:

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker ps --format "{{.Names}} {{.Ports}}"'
```

Registry pulls work because `/root/.docker/config.json` is mounted read-only into the
builds container; you don't need to log in to anything.

---

## Step 4: expose it on the web (web apps only)

DNS first: point `my-app.fandoster.com` at `172.239.117.248`.

Then the nginx site. **Write the file in `sites-available` and symlink it into
`sites-enabled`** — the whole tree follows that convention:

```nginx
server {
    listen 80;
    server_name my-app.fandoster.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name my-app.fandoster.com;
    ssl_certificate /etc/letsencrypt/live/my-app.fandoster.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/my-app.fandoster.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8085;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

If the app uses **WebSockets**, add these two lines inside `location` — nginx strips
hop-by-hop headers, so `proxy_http_version 1.1` alone is not enough:

```nginx
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
```

`$connection_upgrade` is already defined for the whole server in
`/etc/nginx/conf.d/websocket.conf`. Also raise `proxy_read_timeout` past your app's
ping interval, or idle sockets get cut.

Get the certificate and enable the site (certbot writes the cert paths above):

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'certbot certonly --nginx -d my-app.fandoster.com && ln -s /etc/nginx/sites-available/my-app.fandoster.com /etc/nginx/sites-enabled/ && nginx -t && systemctl reload nginx'
```

Never reload without `nginx -t` passing first — a syntax error takes down every site on
the box, not just yours.

---

## Private repositories

Put a personal access token in `clone_token`. It is injected as the userinfo component
of the clone URL for both the clone and the poller's `git ls-remote` probe.

> **⚠ Known broken on modern git.** The code in this repo produces
> `https://<token>@github.com/...` — username only. Git 2.45 and later require **both**
> a username and a password for HTTP Basic auth, and fail with
> `could not read Password for 'https://***@github.com'` (with `GIT_TERMINAL_PROMPT=0`
> it cannot even prompt). The working form is `https://<token>:<token>@github.com/...`,
> i.e. `url.UserPassword(token, token)` in `injectToken`. That fix was applied on the
> server at some point but, like the email code, was never committed — and
> `TestInjectToken` in this repo currently *asserts the broken form*, so it will need
> updating alongside. Until then, expect private-repo clones to fail.

Tokens are **scrubbed from stored logs** — both the raw and percent-encoded forms are
masked, in complete lines, so a token cannot be split across a flush and survive. This
protection covers the clone token specifically. It does **not** cover anything your own
build prints: if your Dockerfile echoes a secret, that secret is in the log verbatim.

---

## Verifying it works

Watch the dashboard at `https://fandoster.com/builds/` — the indicator next to "Recent
Builds" should read `● live`, and a new build appears as a card without a refresh. Click
into it for the streaming log, a step rail with per-step timings, and cancel/re-run.

From a script, the log-free feed is the cheapest check:

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker exec builds sh -c "curl -s -H \"Authorization: Bearer \$BUILDS_PASSWORD\" http://127.0.0.1:8080/api/live"'
```

Then confirm the artifact and the container, not just "build succeeded":

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker images | grep my-app; docker ps --filter name=my-app'
```

---

## When it goes wrong

| Symptom | Cause |
| --- | --- |
| Push does nothing, webhook shows `no matching project` | `repo_url` isn't the HTTPS form, or the branch doesn't match |
| `403` from the API | Missing `X-Builds-Csrf: 1` |
| `401` from the API | No `Authorization: Bearer` / expired session |
| Clone fails on a private repo | Missing or expired `clone_token` |
| `Dockerfile path ... escapes the repository checkout` | `dockerfile_path` has `../` in it |
| Build succeeds, deploy fails after ~2 minutes | `--wait` timeout: the container isn't reaching running/healthy. Check its own logs, not the build log |
| Deploy does nothing, no error | Only one of `deploy_compose_path` / `deploy_service_name` is set — both are required |
| `docker build` exits **125** | Something passed `--progress`. The daemon here is legacy, non-BuildKit, and rejects it |
| Stale image despite a code change | Docker reused a `COPY` layer — set `no_cache` on the project |
| Build stuck `pending` | Another build holds the single worker; check the queue position on the build page |
| Build says `[restart]` and starts over | A redeploy of the build server interrupted it; it re-queues itself, bounded to 2 attempts |

Logs to reach for, in order: the build's own log in the UI → `docker logs <container>`
for a deploy failure → `docker logs builds` for the build server itself.

---

## Gotchas worth internalising

- **`:latest` only.** No per-commit tags, so no rollback target. Rebuild the old commit.
- **One worker.** Builds are serialised; a 30-minute build blocks everything behind it.
- **Shallow clone.** `--depth 1` means checking out an *older* commit fails; the runner
  logs a warning and continues with the branch tip. Fine for "build the push that just
  landed", misleading if you re-run an old webhook build.
- **The build server builds itself.** Project `Build-System` pushes `builds:latest`,
  which makes Watchtower recreate the build server, which interrupts whatever ran next.
  In-flight builds are re-queued rather than lost, but expect the interruption.
- **Deleting a project deletes its builds** (cascade). There's no undo.
- **The registry needs auth.** Anonymous `GET registry.fandoster.com/v2/` returning 401
  is normal, not a fault.
- **Build-completion emails are currently not being sent** — see below. Don't promise
  them to anyone until the code is back in the repo.

---

## Email notifications (currently missing from the code)

`/opt/docker/builds/docker-compose.yml` sets `BUILDS_NOTIFY_EMAIL=danfoster@fandoster.com`,
and build-completion emails genuinely used to work — the host's Postfix log shows
successful deliveries from `builds@fandoster.com` as recently as **2026-07-25 13:35 UTC**:

```
Subject: [builds] windows-fpl ✔ SUCCEEDED (52s)
```

**But that code is not in this repository and never has been.** `sendNotification()` and
the `Runner.NotifyEmail` field existed only in the working copy on the server; no commit
on any branch has ever contained them (`git log --all -S sendNotification` is empty). A
deploy does `git reset --hard origin/main`, so the next one overwrote the binary with a
build that has no mail code in it. Nothing has been sent since.

**Do not "clean up" the env var.** It is the last pointer to the feature. The
surrounding infrastructure is all still in place and working:

- Postfix on the host, listening on `127.0.0.1:25` and `172.17.0.1:25`, with
  `mynetworks` including the Docker bridge ranges `172.17.0.0/16` and `172.18.0.0/16`
- a UFW rule allowing the bridge interface to reach port 25
- an SPF record in Route53 authorising the box: `ip4:172.239.117.248`

So restoring it is a code change only. Two constraints, both learned painfully and
recorded in the Hermes `server-email-notifications` skill on the server:

- **Use `net.Dialer` + `smtp.NewClient`, not `smtp.SendMail`.** The stdlib helper
  negotiates STARTTLS whenever the server advertises it, and Postfix here offers a
  self-signed certificate that Go rejects. Plain TCP to `172.17.0.1:25` is accepted
  because the bridge subnets are in `mynetworks`.
- **Send from `builds@fandoster.com`** and let Postfix relay via the Google Workspace
  MX; the SPF record is what stops it being classed as unsolicited.

Same story for one other fix — see the clone-token warning under
[Private repositories](#private-repositories).

---

## API cheat sheet

All paths are relative to `https://fandoster.com/builds` (or `http://127.0.0.1:8080`
inside the container). `†` = requires `X-Builds-Csrf: 1`.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/health` | Version + liveness. No auth. |
| `GET` | `/api/projects` | List projects (secrets stripped). |
| `POST` | `/api/projects` † | Create a project. |
| `GET` | `/api/projects/{id}` | One project. |
| `PUT` | `/api/projects/{id}` † | Update. Omitted fields keep their value. |
| `DELETE` | `/api/projects/{id}` † | Delete, cascading to its builds. |
| `POST` | `/api/projects/{id}/build` † | Trigger a manual build. |
| `GET` | `/api/projects/{id}/builds` | Build history for a project. |
| `GET` | `/api/builds` | Recent builds, all projects. |
| `GET` | `/api/builds/active` | Pending + running, log-free. |
| `GET` | `/api/live` | Dashboard feed. WebSocket if upgraded, else one JSON snapshot. |
| `GET` | `/api/builds/{id}` | One build. `?meta=1` for status without the log. |
| `GET` | `/api/builds/{id}/log` | Raw log. `?offset=N` to tail, `?download=1` for a file. |
| `GET` | `/api/builds/{id}/events` | SSE stream of log + status for one build. |
| `POST` | `/api/builds/{id}/cancel` † | Cancel queued or running. |
| `POST` | `/api/builds/{id}/rerun` † | New build, same commit. |
| `POST` | `/api/webhook/github` | GitHub push events. No auth (HMAC instead). |

---

## Worked example: `my-app` end to end

1. Repo has a working `Dockerfile` at its root, pushed to
   `https://github.com/FanDoster/my-app`.
2. Create the project with `image_name: my-app` (Step 1).
3. Write `/opt/docker/my-app/docker-compose.yml` binding `127.0.0.1:8085:3000`
   (Step 3, Option C).
4. Set `deploy_compose_path: /opt/docker/my-app/docker-compose.yml` and
   `deploy_service_name: my-app`.
5. Add the GitHub webhook with a secret, and put the same secret on the project
   (Step 2).
6. Point DNS at the box, add the nginx site, run certbot, reload (Step 4).
7. Trigger a manual build and watch it on the dashboard. When it goes green, check
   `https://my-app.fandoster.com`.
8. Push a commit and confirm the webhook builds it on its own.
