# Tailscale Seat Manager

Automate Tailscale user lifecycle management: periodically suspend idle users and let suspended users restore their own access — with identity verified through Okta.

Two self-service restore options are included. Choose whichever fits your stack:

```
┌─────────────────────────┐    ┌──────────────────────────┐    ┌──────────────────────────┐
│   idle-suspender (cron) │    │   slack-restore-bot       │    │   okta-restore-web       │
│                         │    │                          │    │                          │
│  1. List tailnet users  │    │  1. User DMs the bot     │    │  1. User visits page     │
│  2. Find idle >30 days  │    │  2. Bot reads Okta email │    │  2. Signs in via Okta    │
│  3. Suspend via API     │    │     from Slack profile   │    │     (MFA enforced)       │
│  4. Notify Slack channel│    │  3. Matches suspended    │    │  3. Okta returns email   │
└─────────────────────────┘    │     Tailscale account    │    │  4. Account restored     │
                               │  4. Restores on confirm  │    └──────────────────────────┘
                               └──────────────────────────┘
```

---

## How it works

### idle-suspender

A CLI binary designed to run on a schedule (GitHub Actions, cron, k8s CronJob). It:

- Lists all member-type users in your tailnet via the [Tailscale API](https://tailscale.com/api)
- Filters for users whose `status` is `idle` **and** whose last activity was more than 30 days ago
- Skips privileged roles: `owner`, `admin`, `it-admin`, `network-admin`, `billing-admin`, `device-admin`
- Suspends qualifying users and optionally posts a summary to a Slack channel via an incoming webhook

### slack-restore-bot

A Slack bot using [Socket Mode](https://api.slack.com/apis/socket-mode) (no public URL required). It:

- Responds when a suspended user DMs the bot or runs `/tailscale-restore`
- Reads the user's email from their Slack profile — Okta provisions this via SCIM, so it is authoritative
- Looks up that email in the Tailscale user list
- If the account is suspended, presents a Block Kit card with a **Restore my access** button
- Verifies the requester's identity again before calling the restore API
- Users can only ever restore their own account

### okta-restore-web

A small web server that verifies identity via a full **Okta OIDC login** before restoring access. It:

- Presents a simple "Sign in with Okta" landing page — no Slack required
- Redirects the user through your Okta tenant, enforcing whatever MFA policy you have configured
- Receives the user's verified email directly from Okta's userinfo endpoint — the user never types their own email
- Matches the email against suspended Tailscale accounts and restores on confirmation
- Uses HMAC-signed state tokens to prevent CSRF attacks during the OAuth flow
- Adds security headers (`X-Frame-Options`, `CSP`, etc.) on every response

This option provides the **strongest identity guarantee** of the two, since it uses the same Okta authentication challenge that Tailscale itself relies on, including MFA.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Go 1.23+ | For building the binaries |
| Tailscale account | With API key scoped to your tailnet |
| Okta tenant | Used for both Slack SCIM sync (bot) and OIDC login (web) |
| Slack app with Socket Mode | Only required for `slack-restore-bot` |
| Web server / hosting | Only required for `okta-restore-web` (Cloudflare Workers, a VPS, k8s, etc.) |

---

## 1. Tailscale API key

1. Go to **[Tailscale admin console](https://login.tailscale.com/admin/settings/keys)** → **Settings → Personal API keys** (or use an OAuth client for production).
2. Click **Generate API key** and copy it.
3. The key needs the following capabilities for your tailnet:
   - **Users → Read**
   - **Users → Write** (for suspend and restore)

> **Tip:** For production use, create an [OAuth client](https://tailscale.com/kb/1215/oauth-clients) instead of a personal API key. OAuth clients don't expire and can be scoped tightly.

---

## 2. Okta → Slack email sync

The restore bot matches a user's **Slack profile email** against their **Tailscale `loginName`**. This works reliably only if Okta is the source of truth for Slack email addresses. Verify this before deploying:

1. In Okta Admin, go to **Applications → Slack**.
2. Confirm SCIM provisioning is enabled (**Provisioning → To App → Enable**).
3. Confirm **Update User Attributes** is checked — this keeps the Slack `email` field in sync with Okta.
4. Under **Attribute Mappings**, verify that `userName` (or `email`) maps to the user's Okta login (e.g. `user.email`).
5. Check that this same email format is what users log into Tailscale with (typically the same Okta-managed address).

> **Why this matters:** If a user's Slack email differs from their Tailscale `loginName` (e.g. a nickname vs. a formal email), the bot will not find their account. Standardising on the Okta primary email for both avoids this.

---

## 3. Slack app setup

### 3a. Create the app

1. Go to **[api.slack.com/apps](https://api.slack.com/apps)** → **Create New App → From scratch**.
2. Name it something like `Tailscale Access Bot` and choose your workspace.

### 3b. Enable Socket Mode

1. In the left sidebar → **Socket Mode**.
2. Toggle **Enable Socket Mode** on.
3. Click **Generate an app-level token**, give it the scope `connections:write`, name it (e.g. `socket-token`).
4. Copy the `xapp-...` token — this is your `SLACK_APP_TOKEN`.

### 3c. OAuth scopes

1. In the left sidebar → **OAuth & Permissions → Scopes → Bot Token Scopes**.
2. Add:
   - `users:read` — look up user IDs
   - `users:read.email` — read Okta-sourced email from Slack profile
   - `chat:write` — send DM responses

3. Click **Install to Workspace** and copy the `xoxb-...` token — this is your `SLACK_BOT_TOKEN`.

### 3d. Enable events

1. In the left sidebar → **Event Subscriptions**.
2. Toggle **Enable Events** on.
3. Under **Subscribe to bot events**, add:
   - `message.im` — DMs sent to the bot

### 3e. Enable interactivity

1. In the left sidebar → **Interactivity & Shortcuts**.
2. Toggle **Interactivity** on. (No Request URL is needed in Socket Mode — just save.)

### 3f. (Optional) Add a slash command

1. In the left sidebar → **Slash Commands → Create New Command**.
2. Command: `/tailscale-restore`
3. Description: `Restore your Tailscale access`
4. Request URL: put any placeholder — Socket Mode intercepts it before it's used.

---

## 4. Building the binaries

```bash
git clone https://github.com/cryan1988/tailscale-seat-manager.git
cd tailscale-seat-manager

# Build all three binaries into ./bin/
go build -o bin/idle-suspender      ./cmd/idle-suspender/
go build -o bin/slack-restore-bot   ./cmd/slack-restore-bot/
go build -o bin/okta-restore-web    ./cmd/okta-restore-web/
```

---

## 5. Running idle-suspender

### Environment variables

| Variable | Required | Description |
|---|---|---|
| `TAILSCALE_API_KEY` | Yes | Tailscale API key or OAuth client secret |
| `TAILSCALE_TAILNET` | No | Tailnet domain or `-` for default (default: `-`) |
| `SLACK_WEBHOOK_URL` | No | Incoming webhook URL to post a suspension summary |

### Flags

| Flag | Default | Description |
|---|---|---|
| `--idle-days` | `30` | Days without activity before suspending |
| `--dry-run` | `false` | Preview suspensions without applying them |
| `--tailnet` | `-` | Override tailnet (also reads `TAILSCALE_TAILNET`) |
| `--slack-webhook` | — | Override webhook URL (also reads `SLACK_WEBHOOK_URL`) |

### Preview mode (always run this first)

```bash
TAILSCALE_API_KEY=tskey-api-... \
TAILSCALE_TAILNET=yourcompany.com \
./bin/idle-suspender --dry-run
```

### Live run

```bash
TAILSCALE_API_KEY=tskey-api-... \
TAILSCALE_TAILNET=yourcompany.com \
SLACK_WEBHOOK_URL=https://hooks.slack.com/services/... \
./bin/idle-suspender --idle-days 30
```

### Scheduling with GitHub Actions

Create `.github/workflows/suspend-idle-users.yml` in any private repo:

```yaml
name: Suspend idle Tailscale users

on:
  schedule:
    - cron: '0 9 * * 1'   # Every Monday at 09:00 UTC
  workflow_dispatch:       # Allow manual runs from the Actions tab

jobs:
  suspend:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'

      - name: Build
        run: go build -o idle-suspender ./cmd/idle-suspender/

      - name: Suspend idle users
        env:
          TAILSCALE_API_KEY: ${{ secrets.TAILSCALE_API_KEY }}
          TAILSCALE_TAILNET: ${{ secrets.TAILSCALE_TAILNET }}
          SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
        run: ./idle-suspender --idle-days 30
```

Add `TAILSCALE_API_KEY`, `TAILSCALE_TAILNET`, and `SLACK_WEBHOOK_URL` as **repository secrets** under **Settings → Secrets and variables → Actions**.

---

## 6. Running the Slack restore bot

### Environment variables

| Variable | Required | Description |
|---|---|---|
| `TAILSCALE_API_KEY` | Yes | Tailscale API key |
| `TAILSCALE_TAILNET` | No | Tailnet domain or `-` for default |
| `SLACK_BOT_TOKEN` | Yes | `xoxb-...` OAuth bot token |
| `SLACK_APP_TOKEN` | Yes | `xapp-...` app-level socket mode token |

### Start the bot

```bash
TAILSCALE_API_KEY=tskey-api-... \
TAILSCALE_TAILNET=yourcompany.com \
SLACK_BOT_TOKEN=xoxb-... \
SLACK_APP_TOKEN=xapp-... \
./bin/slack-restore-bot
```

The bot connects to Slack over a persistent WebSocket — no inbound firewall rules or public URLs required.

### Running as a long-lived service

For production, run the bot as a systemd service or a container. Example systemd unit:

```ini
[Unit]
Description=Tailscale Slack Restore Bot
After=network.target

[Service]
ExecStart=/usr/local/bin/slack-restore-bot
Restart=always
RestartSec=5
Environment=TAILSCALE_API_KEY=tskey-api-...
Environment=TAILSCALE_TAILNET=yourcompany.com
Environment=SLACK_BOT_TOKEN=xoxb-...
Environment=SLACK_APP_TOKEN=xapp-...

[Install]
WantedBy=multi-user.target
```

Or as a Docker container (add a `Dockerfile` using `FROM golang:1.23-alpine` to build, then `FROM alpine` to run).

---

## 7. End-user experience

Once the bot is running, suspended users interact with it entirely via Slack DM:

1. **User opens a DM with the bot** (or a Slack admin can point users to it when they get suspended).
2. **User sends any message** — the bot immediately checks their Tailscale status.
3. **If suspended**, the bot shows:

   > 🔴 Your Tailscale account (**alice@yourcompany.com**) is currently **suspended**.
   > This usually happens after 30 days of inactivity. Click the button below to restore your access.
   >
   > [ **Restore my access** ]   [ Dismiss ]

4. **User clicks Restore my access** — the bot re-verifies their Slack identity, calls the Tailscale API, and updates the message:

   > ✅ Done! Your Tailscale account (**alice@yourcompany.com**) has been restored. Open Tailscale and reconnect — you should be back on the tailnet.

The user then reconnects in the Tailscale client without any IT admin involvement.

---

## 7. Running the Okta restore web app

This is an alternative to the Slack bot. Use it when you want a self-contained web-based restore flow that doesn't depend on Slack.

### 7a. Create an Okta OIDC application

1. In Okta Admin, go to **Applications → Applications → Create App Integration**.
2. Choose **OIDC - OpenID Connect** → **Web Application**.
3. Name it (e.g. `Tailscale Restore`).
4. Under **Grant type**, ensure **Authorization Code** is checked.
5. Set **Sign-in redirect URIs** to your callback URL:
   ```
   https://restore.yourcompany.com/auth/callback
   ```
   Use `http://localhost:8080/auth/callback` for local testing.
6. Under **Assignments**, assign the application to **Everyone** (or a specific group if you want to restrict who can self-restore).
7. Save and note the **Client ID** and **Client Secret**.
8. Note your **Okta domain** from the top-right of the Okta Admin console (e.g. `yourcompany.okta.com`).

### 7b. Enable MFA on the Okta application (recommended)

1. In Okta Admin, go to **Security → Authentication Policies**.
2. Create or edit a policy and add a rule that requires MFA for the Tailscale Restore app.
3. Assign that policy to the `Tailscale Restore` application under **Applications → [app] → Sign On**.

This ensures that even if a user's password is compromised, they cannot restore their own Tailscale access without passing MFA.

### 7c. Environment variables

| Variable | Required | Description |
|---|---|---|
| `TAILSCALE_API_KEY` | Yes | Tailscale API key |
| `TAILSCALE_TAILNET` | No | Tailnet domain or `-` (default: `-`) |
| `OKTA_DOMAIN` | Yes | Your Okta domain, e.g. `yourcompany.okta.com` |
| `OKTA_CLIENT_ID` | Yes | Client ID from the OIDC app |
| `OKTA_CLIENT_SECRET` | Yes | Client secret from the OIDC app |
| `OKTA_REDIRECT_URI` | Yes | Full callback URL, e.g. `https://restore.yourcompany.com/auth/callback` |
| `SESSION_SECRET` | Yes | Random string ≥ 32 characters for signing CSRF state tokens |
| `PORT` | No | HTTP listen port (default: `8080`) |

Generate a secure `SESSION_SECRET`:

```bash
openssl rand -base64 32
```

### 7d. Run locally for testing

```bash
export TAILSCALE_API_KEY=tskey-api-...
export TAILSCALE_TAILNET=yourcompany.com
export OKTA_DOMAIN=yourcompany.okta.com
export OKTA_CLIENT_ID=0oaxxxxxxxxxxxxxxxxx
export OKTA_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
export OKTA_REDIRECT_URI=http://localhost:8080/auth/callback
export SESSION_SECRET=$(openssl rand -base64 32)

./bin/okta-restore-web
# Open http://localhost:8080 in a browser
```

### 7e. Deploy to production

For production, run behind a TLS-terminating reverse proxy (nginx, Caddy, AWS ALB, etc.) or deploy as a container. The binary itself listens on plain HTTP and sets the `Secure` cookie flag only when the incoming request arrived over HTTPS (detected via `r.TLS`). If your proxy terminates TLS before the binary, set `OKTA_REDIRECT_URI` to your `https://` URL.

Example with Caddy (automatic TLS):

```
restore.yourcompany.com {
    reverse_proxy localhost:8080
}
```

Or as a Docker container:

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /okta-restore-web ./cmd/okta-restore-web/

FROM alpine:3.20
COPY --from=build /okta-restore-web /okta-restore-web
ENTRYPOINT ["/okta-restore-web"]
```

### 7f. End-user experience

1. User visits `https://restore.yourcompany.com`
2. Clicks **Sign in with Okta** — redirected to their company Okta login
3. Completes Okta authentication (password + MFA if configured)
4. Redirected back automatically — no form to fill in
5. If their account is suspended, they see a confirmation and their access is restored
6. They open the Tailscale app and reconnect

---

## Roles protected from auto-suspension

The idle-suspender never suspends users with elevated roles, regardless of inactivity:

| Role | Description |
|---|---|
| `owner` | Tailnet owner |
| `admin` | Full administrator |
| `it-admin` | IT administrator |
| `network-admin` | Network administrator |
| `billing-admin` | Billing administrator |
| `device-admin` | Device administrator |

---

## Project structure

```
tailscale-seat-manager/
├── cmd/
│   ├── idle-suspender/
│   │   └── main.go          # Periodic suspension job
│   ├── slack-restore-bot/
│   │   └── main.go          # Slack self-service restore bot
│   └── okta-restore-web/
│       └── main.go          # Okta OIDC web self-service restore
├── internal/
│   └── tsapi/
│       └── client.go        # Tailscale API client (shared)
├── go.mod
├── go.sum
└── README.md
```

---

## Security notes

- The Slack bot reads the requester's email **from their Slack profile**, not from a message they type. A user cannot impersonate another user to restore a different account.
- Identity re-verification happens at button-click time, not just at message time, to prevent TOCTOU issues with message replay.
- Store all tokens as secrets (GitHub Actions secrets, a secrets manager, or environment variables injected at runtime) — never commit them to source control.
- Rotate your Tailscale API key periodically, or use an OAuth client with a short-lived token lifetime.

---

## Troubleshooting

**Bot says "No Tailscale account found" for a valid user**
- Confirm the user's Slack profile email matches their Tailscale `loginName` exactly (case-insensitive comparison is already applied).
- In Okta Admin → the user's profile → check their `email` attribute matches what they use to log into Tailscale.
- Trigger a manual Okta → Slack SCIM push: **Okta Admin → Applications → Slack → Push Now**.

**idle-suspender suspends nobody even though idle users exist**
- Run with `--dry-run` and check the output.
- The `idle` status in the Tailscale API triggers after 28 days; the suspender additionally checks `lastSeen > 30 days`. Users between 28–30 days will show as `idle` in the admin console but will not yet be suspended.

**Bot stops responding after a network interruption**
- The slack-go socket mode client reconnects automatically. If it does not, restart the process. Running under systemd with `Restart=always` handles this automatically.
