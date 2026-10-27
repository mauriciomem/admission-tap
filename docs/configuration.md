# Configuration

The server is configured with command-line flags. In the cluster, they go in `args` in `deploy/10-server.yaml`. After editing, re-run `scripts/deploy.sh`.

The manifest only sets `-capture`, from the script's `CAPTURE` variable. Everything else uses the defaults below.


## Listeners and TLS

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8443` | Webhook listen address (HTTPS). |
| `-tls-cert` | `/etc/webhook/certs/tls.crt` | TLS certificate. |
| `-tls-key` | `/etc/webhook/certs/tls.key` | TLS private key. |
| `-api-addr` | `:8080` | Query API listen address (plain HTTP). Empty disables the API. |
| `-api-token-file` | `/etc/webhook/api-token/token` | File holding the API bearer token. |

About `-api-token-file`:

- If the file is missing or empty, the server refuses to start.
- If the flag is set to an empty string, the API runs without authentication, and the server logs a warning.


## Capture

| Flag | Default | Description |
|---|---|---|
| `-capture` | `true` | Record the admission calls this server receives to stdout and the store. Which calls arrive is decided by the webhook configurations. With `false`, nothing is recorded. |
| `-capture-stdout` | `full` | What each record writes to stdout: `full`, `summary` or `off`. |
| `-redact-secrets` | `true` | Redact core/v1 Secret values and the last-applied annotation. |
| `-capture-max-entries` | `1000` | Maximum number of records kept in memory. |
| `-capture-max-bytes` | `33554432` (32 MiB) | Maximum total size of records kept in memory. |

`-capture=false` only stops recording. The webhooks keep answering. To stop the API server from calling `/capture` at all, deploy with `CAPTURE=false` (see [Deploying](deploying.md)).

[Capturing](capturing.md) explains redaction, the stdout modes and eviction.


## Shutdown

| Flag | Default | Description |
|---|---|---|
| `-shutdown-delay` | `5s` | On SIGTERM, `/healthz` on the webhook port starts returning 503 and the server keeps serving for this long, then shuts down gracefully (up to 10s more). `0` shuts down at once. |

During a rolling update the API server keeps sending calls to the old Pod until the endpoint removal reaches it. The delay covers that gap, so calls aren't dropped. The image has no shell, so a `preStop` sleep isn't possible. `terminationGracePeriodSeconds` in `deploy/10-server.yaml` is 30, which covers the delay plus the shutdown. If you raise `-shutdown-delay`, raise it too.


## Memory

The container has a 256Mi memory limit. With the default store limits, that's plenty.

If you raise `-capture-max-bytes`, raise the limit in `deploy/10-server.yaml` too.
