# Records

Every call the server receives is recorded, from any of its three webhooks. This page describes what a record holds, what's redacted, and where records go.

For the webhooks themselves, see [Capture](capture.md) and [Mutate and validate](mutate-validate.md). To read records back, see the [Query API](query-api.md).


## What a record is

One record per HTTP call to `/capture`, `/mutate` or `/validate`. It holds the HTTP details, a summary lifted out of the AdmissionReview for filtering, and the review the server received and the one it sent back.

The `webhook` field tells you which webhook the call came through: `capture`, `mutate` or `validate`. A Pod created in a namespace with both labels gives three records, one per webhook.

The server records whatever it receives. Which calls arrive is decided only by the webhook configurations and the namespace labels.

Failed and malformed calls are recorded too: a wrong method, a wrong content type, invalid JSON, or a body over 3 MiB. They have a non-200 `status` and an `error` message. A body over 3 MiB isn't kept.


## Record format

Fields in order. Those marked "summary" are empty when the body couldn't be parsed:

| Field | Description |
|---|---|
| `id` | Increasing number, unique until the server restarts. |
| `time` | When the call arrived, in UTC (RFC 3339). |
| `durationMs` | How long the server took to answer, in milliseconds. |
| `webhook` | `capture`, `mutate` or `validate`. |
| `status` | HTTP status the server answered with. 200 for every answered review, including denials. |
| `error` | Why the call failed. Only on failed calls. |
| `http` | `method`, `path`, `rawQuery`, `remoteAddr`, `headers`, `contentLength`, and `tls` (`version`, `cipherSuite`, `serverName`). |
| `uid` | Summary: the review's `request.uid`. |
| `operation` | Summary: `CREATE`, `UPDATE`, `DELETE` or `CONNECT`. |
| `kind` | Summary: `group`, `version` and `kind` of the object, such as `Pod`. |
| `resource` | Summary: `group`, `version` and `resource`, such as `pods`. |
| `subResource` | Summary: such as `exec` or `status`. Only for subresource calls. |
| `namespace`, `name` | Summary: where the object lives, and its name. |
| `username`, `groups` | Summary: who made the request. |
| `dryRun` | Summary: `true` for `--dry-run=server` requests. |
| `allowed` | The server's answer. Missing on failed calls. |
| `request` | The AdmissionReview received. If it wasn't valid JSON, it's stored as a JSON string. |
| `response` | The AdmissionReview sent back. Missing on failed calls. |

Empty fields are left out of the JSON.

There are full examples in [A capture record](capture.md#a-capture-record), [A mutate record](mutate-validate.md#a-mutate-record) and [A validate record](mutate-validate.md#a-validate-record).

### Summaries

`?summary=true` on the query API, and `-record-stdout=summary`, use the same payload-free view: `id`, `time`, `webhook`, `status`, `error`, `uid`, `operation`, `kind`, `resource`, `subResource`, `namespace`, `name`, `username`, `groups`, `dryRun`, `allowed` and `durationMs`. No `http`, `request` or `response`.


## Redaction

With `-redact-secrets=true`, the default, core/v1 Secrets are redacted in `request.object` and `request.oldObject`:

- Each value in `data` and `stringData` becomes `<redacted>`. The keys stay.
- The `kubectl.kubernetes.io/last-applied-configuration` annotation becomes `<redacted>`, because `kubectl apply` stores the whole manifest there.

If redaction fails, the whole `request` is stored as `"<redaction failed>"` rather than in clear.

The `Authorization`, `Proxy-Authorization`, `Cookie` and `X-Api-Key` HTTP headers are always redacted, whatever the setting.

Nothing else is redacted. Secrets in env vars, ConfigMaps, exec commands, annotations or custom resources are recorded as they are.

Only the record is redacted. The webhook logic always sees the original request.


## Where records go

### The in-memory store

Every record is kept in a bounded, in-memory store, which the [query API](query-api.md) serves.

The store is first in, first out. When it goes over `-record-max-entries` (1000) or `-record-max-bytes` (32 MiB), the oldest records are evicted. A record larger than the byte limit is counted but not stored.

Everything is lost when the Pod restarts.

### Stdout

Each record is also written to stdout as one JSON line. `-record-stdout` controls this:

- `full`, the default, writes the whole record.
- `summary` writes the [summary](#summaries) view.
- `off` writes nothing.

The store always keeps full records, whatever the stdout mode.

The server's own logs go to stderr, also as JSON. Record lines are the ones with a `webhook` field.

Under load, stdout lines can appear slightly out of id order. The store is always in order.

### Reading the logs with jq

`select(.webhook)` keeps record lines and drops the server's own logs:

```bash
kubectl -n admission-tap logs deploy/admission-tap \
  | jq -c 'select(.webhook) | {id, webhook, operation, resource: .resource.resource, subResource, namespace, name, username, allowed}'
```

Only one webhook, for example validate:

```bash
kubectl -n admission-tap logs deploy/admission-tap | jq -c 'select(.webhook == "validate")'
```

Save everything to a file:

```bash
kubectl -n admission-tap logs deploy/admission-tap | jq -c 'select(.webhook)' > records.jsonl
```


## Turning recording off

To keep the webhooks running but record nothing, start the server with `-record=false`. Add it to the container in `deploy/10-server.yaml`:

```yaml
      containers:
        - name: webhook
          image: __IMAGE__
          args: ["-record=false"]
```

Then re-run `scripts/deploy.sh`. The webhooks keep answering as before, but nothing is written to stdout or kept in the store. The query API still runs, stays empty, and `/api/stats` shows `"recordEnabled": false`.

To stop the webhooks from being called at all, don't register them instead: see [Choosing which webhooks to register](deploying.md#choosing-which-webhooks-to-register).
