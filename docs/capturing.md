# Capturing

## Opting a namespace in

Nothing is captured until you label a namespace:

```bash
kubectl label namespace <ns> admission-tap/capture=enabled
```

To opt it out again, remove the label. The trailing `-` means "remove":

```bash
kubectl label namespace <ns> admission-tap/capture-
```

This label is independent of `admission-tap=enabled`, which only turns on the example mutate and validate webhooks. A namespace can have either, both, or neither.

The `admission-tap` namespace is never captured, even if labelled. That way the server can always restart itself.


## What is captured

In a labelled namespace, the `admission-tap-capture` webhook receives every write:

- All resources, in every API group, including custom resources.
- All subresources, such as `pods/exec`, `pods/status` and `deployments/scale`.
- All operations: CREATE, UPDATE, DELETE and CONNECT. CONNECT covers `kubectl exec`, `attach` and `port-forward`.

Changes to the labelled Namespace object itself are captured too.

Records follow the object's namespace, not the requester's. For example, when you create a Deployment in a labelled namespace, the Pods that the ReplicaSet controller creates there are captured as well.

Failed and malformed calls are also recorded: a wrong method, a wrong content type, invalid JSON, or a body over 3 MiB. They have a non-200 `status` and an `error` message.

Calls to `/mutate` and `/validate` are recorded the same way, in any namespace labelled `admission-tap=enabled`. They don't need the capture label.

If the server runs with `-capture=false`, nothing is recorded at all.


## What isn't captured

Other namespaces, and cluster-scoped objects other than Namespaces: Nodes, ClusterRoles, CustomResourceDefinitions, PersistentVolumes and so on.

These are never sent to any webhook, by Kubernetes design:

- Reads: GET, LIST and WATCH. Admission only covers writes. To see reads, use the API server audit log.
- Webhook configurations and admission policies, and their bindings. This way a broken webhook can't stop you from fixing it.
- Requests rejected by authentication or authorization. They never reach admission.


## Things to know

### Turning the label on and off

Capture starts and stops almost immediately after you add or remove the label.

For changes to the Namespace object itself, the API server looks at the new labels. So the update that adds the label is captured, and the one that removes it isn't.

### The same call recorded more than once

The demo namespace has both labels. A Pod created there is recorded three times: by mutate, by validate and by capture.

Each record has a `webhook` field, so you can tell them apart or filter with `webhook=capture`.

### Capture's `allowed` is always true

Validating webhooks run in parallel. The capture record's `allowed` is capture's own answer, which is always `true`.

It doesn't tell you whether another webhook denied the request. Look at the `validate` record for that.

### Noise

Busy namespaces produce a steady stream of Events, Pod status updates and controller writes. The store is bounded, so older records get evicted. Filter narrowly and query soon, or raise the limits (see [Configuration](configuration.md)).


## Redaction

With `-redact-secrets=true`, the default, core/v1 Secrets are redacted in `request.object` and `request.oldObject`:

- Each value in `data` and `stringData` becomes `<redacted>`.
- The `kubectl.kubernetes.io/last-applied-configuration` annotation becomes `<redacted>`, because `kubectl apply` stores the whole manifest there.

`stringData` never shows up in a capture in practice: the API server converts it to `data` before validating webhooks run. Redaction still covers it, for safety.

If redaction fails, the whole request body is stored as `"<redaction failed>"` rather than in clear.

The `Authorization`, `Proxy-Authorization`, `Cookie` and `X-Api-Key` HTTP headers are always redacted.

Nothing else is redacted. Secrets in env vars, ConfigMaps, exec commands, annotations or custom resources are recorded as-is.

Only the record is redacted. The webhook logic always sees the original request.


## Record format

Each call produces one record:

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

A few details:

- `subResource` appears for subresource calls, such as `exec`.
- `error` appears only on failed calls.
- The summary fields, `uid` to `allowed`, are empty if the body couldn't be parsed.
- `request` is the raw body. If the body wasn't valid JSON, it's stored as a JSON string.


## Where records go

### The in-memory store

Every record is kept in a bounded, in-memory store, which the [query API](query-api.md) serves.

The store is first in, first out. When it goes over `-capture-max-entries` or `-capture-max-bytes`, the oldest records are evicted. A record larger than the byte limit is counted but not stored.

Everything is lost when the Pod restarts.

### Stdout

By default, each record is also written to stdout as one JSON line. `-capture-stdout` controls this:

- `full`, the default, writes the whole record.
- `summary` writes the same payload-free view as `?summary=true` on the [query API](query-api.md): id, time, webhook, status, `error`, `uid`, operation, kind, resource, namespace, name, `username`, `groups`, `dryRun`, `allowed` and duration. No `http`, `request` or `response`.
- `off` writes nothing.

The store always keeps full records, whatever the stdout mode.

The server's own logs go to stderr, also as JSON. Capture lines are the ones with a `webhook` field.

Under load, stdout lines can appear slightly out of id order. The store is always in order.

### Reading the logs with jq

`select(.webhook)` keeps capture lines and drops the server's own logs:

```bash
kubectl -n admission-tap logs deploy/admission-tap \
  | jq -c 'select(.webhook) | {id, webhook, operation, resource: .resource.resource, subResource, namespace, name, username, allowed}'
```

Save everything to a file:

```bash
kubectl -n admission-tap logs deploy/admission-tap | jq -c 'select(.webhook)' > captures.jsonl
```


## Things worth trying

With the demo namespace in place, run these and watch the logs:

```bash
# CONNECT on pods/exec
kubectl -n admission-tap-demo exec web-ok -- id

# A request with dryRun: true
kubectl -n admission-tap-demo run dry --image=nginx:1.27 --dry-run=server

# Pods created by the replicaset-controller
kubectl -n admission-tap-demo create deployment web --image=nginx:1.27

# An UPDATE with oldObject
kubectl -n admission-tap-demo label pod web-ok tier=frontend
```
