# Capturing

## What is captured

The `admission-tap-capture` webhook receives every admission call in the cluster: all API groups, resources and subresources, all operations (CREATE, UPDATE, DELETE, CONNECT), cluster-scoped and namespaced, in every namespace except `admission-tap`. The mutate and validate webhooks are recorded too.

Failed and malformed calls are also recorded, such as a wrong method, wrong content type, invalid JSON, or a body over 3 MiB. They have a non-200 `status` and an `error` message.

## What is never captured

These limits come from Kubernetes itself, not from this webhook:

- GET, LIST and WATCH. Admission only covers writes. To see reads, use the API server audit log.
- Webhook configurations and admission policy objects (`ValidatingWebhookConfiguration`, `MutatingWebhookConfiguration`, `ValidatingAdmissionPolicy`, `MutatingAdmissionPolicy` and their bindings). Kubernetes never sends these to webhooks, so a broken webhook can't stop you from fixing it.
- Requests rejected by authentication or authorization. They never reach admission.

Also note that validating webhooks run in parallel. The capture record's `allowed` is capture's own answer, which is always `true`. It doesn't tell you whether another webhook denied the request. Look at the `validate` record for that.

## Redaction

With `-redact-secrets=true` (the default), each value in a core/v1 Secret's `data` and `stringData` is replaced with `<redacted>` in `request.object` and `request.oldObject`. The `kubectl.kubernetes.io/last-applied-configuration` annotation is redacted too, because `kubectl apply` stores the whole manifest there. If redaction fails, the whole request body is replaced with `"<redaction failed>"` instead of being stored in clear. The `Authorization`, `Proxy-Authorization`, `Cookie` and `X-Api-Key` HTTP headers are always redacted.

Nothing else is redacted. Secrets in env vars, ConfigMaps, exec commands, annotations or custom resources end up in the records as-is. Only the record is redacted. The webhook logic always sees the original request.

## Record format

Each record is added to the in-memory store and, by default, written to stdout as one JSON line. With `-capture-stdout=summary` stdout gets only the summary fields plus `error`; with `off` it gets nothing. The store always keeps the full record. The server's own logs go to stderr, also as JSON. Capture lines are the ones with a `webhook` field. Under load, stdout lines can appear slightly out of id order; the store is always in order.

```json
{
  "id": 42,
  "time": "2026-10-26T10:00:00.123Z",
  "durationMs": 0.41,
  "webhook": "validate",
  "status": 200,
  "http": {
    "method": "POST", "path": "/validate", "rawQuery": "timeout=5s",
    "remoteAddr": "10.0.0.1:51234", "headers": { "...": [] }, "contentLength": 2048,
    "tls": {"version": "TLS1.3", "cipherSuite": "TLS_AES_128_GCM_SHA256", "serverName": "admission-tap.admission-tap.svc"}
  },
  "uid": "705ab4f5-...", "operation": "CREATE",
  "kind": {"group": "", "version": "v1", "kind": "Pod"},
  "resource": {"group": "", "version": "v1", "resource": "pods"},
  "namespace": "admission-tap-demo", "name": "web-latest",
  "username": "kubernetes-admin", "groups": ["system:authenticated"],
  "dryRun": false, "allowed": false,
  "request":  { "...": "AdmissionReview received" },
  "response": { "...": "AdmissionReview sent" }
}
```

`error` appears only on failed calls. The summary fields (`uid` to `allowed`) are empty if the body couldn't be parsed. `request` holds the raw body, or the body as a JSON string if it wasn't valid JSON.

The store is a FIFO. When either `-capture-max-entries` or `-capture-max-bytes` is exceeded, the oldest records are evicted. A record larger than the byte limit is counted but not stored. Everything is lost when the Pod restarts.

## Query API

The API is plain HTTP on port 8080 (Service port 80). Every `/api/*` request needs `Authorization: Bearer <token>`; without it you get 401 with `WWW-Authenticate: Bearer`. `/` and `/healthz` are open. Responses are pretty-printed JSON.

| Method | Path | Returns |
|---|---|---|
| GET | `/` | Help text |
| GET | `/healthz` | `ok` |
| GET | `/api/stats` | Counters: stored, storedBytes, total, evicted, denied, errors, byWebhook, byOperation, and the limits |
| GET | `/api/captures` | `{"count": N, "items": [...]}`, newest first |
| GET | `/api/captures/{id}` | One full record, or 404 if it never existed or was evicted |
| DELETE | `/api/captures` | Clears the store and returns `{"removed": N}`. Counters in `/api/stats` are kept |

Filters for `/api/captures` are optional and combined with AND. Matching is case-insensitive.

- `webhook`: `mutate`, `validate` or `capture`.
- `namespace`, `name`, `kind`, `resource`, `subresource`, `operation`: exact match. `kind` is the Kind (`Pod`), `resource` is the plural resource (`pods`).
- `user`: substring of the username.
- `allowed=true|false`. Records with no response (errors) never match.
- `errors=true`: only failed calls.
- `since`: a record id (returns newer ids) or an RFC3339 time.
- `limit`: default 50. Full records are capped at 100; with `summary=true` the cap is the store size (`-capture-max-entries`). Larger values are clamped, not rejected.
- `summary=true`: drop the HTTP metadata and payloads.

Invalid values return 400 with `{"error": "..."}`.

## Examples

These assume `make api` is running and `export TOKEN=$(make -s token)`. From inside the cluster, use `http://admission-tap.admission-tap.svc` instead of `localhost:8080` (see the README for passing the token to a curl Pod).

```bash
A="Authorization: Bearer $TOKEN"

# What's happening right now
curl -s -H "$A" 'localhost:8080/api/captures?summary=true&limit=20'

# Who deleted a Secret
curl -s -H "$A" 'localhost:8080/api/captures?kind=Secret&operation=DELETE&summary=true'

# Commands run with kubectl exec
curl -s -H "$A" 'localhost:8080/api/captures?subresource=exec&webhook=capture' | jq -c '.items[].request.request.object.command'

# Decode the patch from the mutating webhook
curl -s -H "$A" 'localhost:8080/api/captures?webhook=mutate&limit=1' | jq -r '.items[0].response.response.patch' | base64 -d

# Poll for new records since id 120
curl -s -H "$A" 'localhost:8080/api/captures?since=120&summary=true'

# Calls that failed
curl -s -H "$A" 'localhost:8080/api/captures?errors=true'
```

From the logs instead of the API:

```bash
kubectl -n admission-tap logs deploy/admission-tap \
  | jq -c 'select(.webhook) | {id, webhook, operation, resource: .resource.resource, subResource, namespace, name, username, allowed}'

# Save everything to a file
kubectl -n admission-tap logs deploy/admission-tap | jq -c 'select(.webhook)' > captures.jsonl
```

Some things worth trying while you watch:

```bash
kubectl -n admission-tap-demo exec web-ok -- id                                   # CONNECT on pods/exec
kubectl -n admission-tap-demo run dry --image=nginx:1.27 --dry-run=server         # dryRun: true
kubectl -n admission-tap-demo create deployment web --image=nginx:1.27            # Pods arrive as the replicaset-controller
kubectl -n admission-tap-demo label pod web-ok tier=frontend                      # UPDATE with oldObject
```
