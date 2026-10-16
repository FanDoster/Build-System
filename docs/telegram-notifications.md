# Telegram notifications

## Project controls

Open **Project → Settings → Notifications**. **Telegram notifications** enables build
results and related agent issues for that project. Uncheck it and save to mute the
project. Check it and save to unmute. This does not change build-completion email.
A shared agent may still produce alerts for other projects that have notifications on.

Muting atomically cancels pending, unsent Build-System outbox events. Unmuting does not
replay those canceled events, and stale in-memory work cannot revive them. Messages
already accepted into the relay queue or in
flight to Telegram cannot be recalled through the project checkbox. The relay
administrator can revoke the dedicated identity to cancel its pending relay jobs;
this affects every project using that identity and cannot recall in-flight requests.

The setting defaults to enabled for existing projects migrated by the server and for
new projects created through the API. Direct Go model creation keeps the normal
boolean zero value, disabled.

On first migration, existing terminal build history is marked as already observed;
initial setup does not send that history. Builds still active at migration can notify
their future outcome after setup is ready. A missing server key or an explicit relay
response showing no configured group-only, title-suppressed route suppresses unsent
intents. Connecting the group later does not replay events from that disabled state.

Temporary network failures, HTTP 429 and relay 5xx errors do not count as disabled
setup. They retain retryable intent for up to 24 hours. This allows temporary-outage
recovery without turning initial setup or a deliberately disconnected route into a
notification backlog.

API examples (use operator authentication and `X-Builds-Csrf: 1`):

```http
PUT /api/projects/7
Content-Type: application/json

{"telegram_notifications":false}
```

Send `true` to unmute. Omit the property to keep its current value. On
`POST /api/projects`, omission defaults to `true`; explicit `false` starts muted.
The checkbox serializes both checked and unchecked states as JSON booleans.
Agent credentials cannot create projects, change this setting or open project settings.
Polling, queue profiles, project secrets and email retain their existing behavior.

## Server setup

1. The owner creates the intended Telegram group.
2. The owner adds the notification bot to that group and permits it to send messages.
3. The relay operator provisions a dedicated Build-System notification identity and
   routes it **only** to that group. A missing or invalid group must reject delivery.
   It must never fall back to a personal chat.
4. Install that identity's key in a server-only file. Keep it outside Git and the
   build workspace. Mount it read-only into the build-server container.
5. Set the environment variables below and restart the server.
6. Verify the dedicated group route and check the readiness status in project settings.
   Check an actual notification in the intended group to verify delivery end to end.

| Variable | Default | Purpose |
| --- | --- | --- |
| `BUILDS_TELEGRAM_KEY_FILE` | unset | File containing the dedicated group-routed relay identity key. Unset disables Telegram delivery. |
| `BUILDS_TELEGRAM_URL` | `https://notify.fandoster.com` | Notification relay base URL. |
| `BUILDS_PUBLIC_URL` | empty | Public build-server URL for links, for example `https://fandoster.com/builds`. |

Never place the key in a project, build-agent configuration, browser, log or API
response. Do not expose the server key path or recipient identifiers through the UI.
The build server uses a dedicated group-only identity, not a personal notification key.

### Local relay commands

Run these only on the notification-relay host as its service account or root, using
the relay's normal service environment and database. Do not print or source secret
configuration into an agent session. The following IDs, chat ID and key path are
placeholders; never paste their real values into project settings or the browser.

```sh
# Uses the normal service environment; prints only the public bot identity.
sudo -u agent-notify /usr/local/bin/agent-notify bot-info

# The absolute private key file must not exist. The command creates it mode 0600.
sudo -u agent-notify /usr/local/bin/agent-notify create-group-agent build-system-group /ABSOLUTE/PRIVATE/KEY_FILE

# Use the returned identity ID and the owner's canonical negative group chat ID.
sudo -u agent-notify /usr/local/bin/agent-notify bind-group AGENT_ID NEGATIVE_GROUP_CHAT_ID --suppress-title
```

`bot-info` needs `NOTIFY_TELEGRAM_TOKEN` loaded through the normal service environment
(for example its systemd `EnvironmentFile`), not a token argument. The binding command
checks negative numeric chat-ID syntax; it does not verify Telegram membership or
send a test message. Names cannot be reused after revocation. Provision only the
created dedicated key to Build-System, never the bot token or enrollment secret.

With its own key, Build-System requires the relay's `GET /v1/route` response to contain
all three booleans exactly:

```json
{"configured":true,"group":true,"preserve_title":false}
```

A personal key cannot pass this check. Suppressing the sender title lets build-result
messages start directly with their result heading. No HTTP caller can select a
recipient or alter this policy.

To disconnect new relay submissions, use local `clear-group AGENT_ID`. Already
accepted jobs keep their original group route and title policy. To cancel pending
relay jobs, use local `revoke AGENT_ID`; accepted requests already in flight cannot be
recalled. Never roll back to an older relay binary with an active group-only identity
or pending group job: older binaries do not understand the routing boundary. Follow
the relay README's revoke-and-verify rollback procedure or roll forward instead.

## Readiness and limits

Project settings shows **Telegram group connected** only when the server's callback
reports a verified dedicated group route. Otherwise it shows **Telegram group not
connected** and setup help. Merely finding a key file is not sufficient verification.

Operators can also request:

```http
GET /api/notifications/telegram
```

The response contains only `{"configured":true}` or `{"configured":false}`. Agent
credentials cannot read it. A connected route confirms relay routing configuration,
not Telegram membership, permission to post, or delivery.

A successful submission means the relay **saved the message to its queue**. It is not
confirmation that Telegram accepted it. Build-System checks the relay receipt:
`sent` confirms Telegram accepted the send; `failed`, `discarded` or `expired` does not.
Even `sent` is not confirmation that anyone read the message. A lost response after
Telegram accepts a send can still cause a rare duplicate. The worker keeps stable
idempotency keys and payloads across relay submission retries to reduce duplicates.

Enabling a project's checkbox without a configured server key and verified group
route does not deliver messages. Check server and relay setup rather than repeatedly
toggling the project setting. Telegram delivery is separate from SMTP email;
`BUILDS_NOTIFY_EMAIL` controls only email.

Telegram does not support custom font sizes. Build-result headings use a **bold first
line** instead of big-font markup. Agent messages begin with 🤖 and remain plain text
so doctor commands, paths and remedies retain their literal characters.
