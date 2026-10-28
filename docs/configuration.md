# Configuration

The server is configured with command-line flags.

`deploy/10-server.yaml` sets no args, so in the cluster every flag uses the default below. Recording is on.


## Setting flags

Add an `args` list to the `webhook` container in `deploy/10-server.yaml`, then re-run `scripts/deploy.sh`. For example, to write only summaries to stdout and keep more records:

```yaml
      containers:
        - name: webhook
          image: __IMAGE__
          args: ["-record-stdout=summary", "-record-max-entries=5000"]
```

Changing the args changes the Pod template, so the server restarts with the new flags.


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


## Recording

| Flag | Default | Description |
|---|---|---|
| `-record` | `true` | Record every admission call the server receives, from all three webhooks, to stdout and the store. With `false`, nothing is recorded. |
| `-record-stdout` | `full` | What each record writes to stdout: `full`, `summary` or `off`. Any other value stops the server at startup. |
| `-redact-secrets` | `true` | Redact core/v1 Secret values and the last-applied annotation. |
| `-record-max-entries` | `1000` | Maximum number of records kept in memory. |
| `-record-max-bytes` | `33554432` (32 MiB) | Maximum total size of records kept in memory. |

`-record=false` only stops recording. The webhooks keep answering. To stop the API server from calling a webhook at all, don't register it: deploy with `CAPTURE=false` or `MUTATE_VALIDATE=false` (see [Deploying](deploying.md#choosing-which-webhooks-to-register)).

[Records](records.md) explains redaction, the stdout modes and eviction.


## Shutdown

| Flag | Default | Description |
|---|---|---|
| `-shutdown-delay` | `5s` | On SIGTERM, `/healthz` on the webhook port starts returning 503 and the server keeps serving for this long, then shuts down gracefully (up to 10s more). `0` shuts down at once. |

During a rolling update the API server keeps sending calls to the old Pod until the endpoint removal reaches it. The delay covers that gap, so calls aren't dropped. The image has no shell, so a `preStop` sleep isn't possible. `terminationGracePeriodSeconds` in `deploy/10-server.yaml` is 30, which covers the delay plus the shutdown. If you raise `-shutdown-delay`, raise it too.


## Memory

The container has a 256Mi memory limit. With the default store limits, that's plenty.

If you raise `-record-max-bytes`, raise the limit in `deploy/10-server.yaml` too.
