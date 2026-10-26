# How it works

## Where admission sits

Every `kubectl` command is an HTTPS request to the API server. A write goes through these steps before it reaches etcd:

```
request → authentication → authorization → mutating admission → schema validation → validating admission → etcd
```

Mutating webhooks run first and can change the object by returning a patch. Validating webhooks run after all mutations and can only allow or deny. That's why the validating webhooks here see the label added by the mutating one.

## The AdmissionReview round trip

For each matching request, the API server POSTs an `AdmissionReview` to the webhook:

```json
{
  "apiVersion": "admission.k8s.io/v1",
  "kind": "AdmissionReview",
  "request": {
    "uid": "705ab4f5-...",
    "kind": {"group": "", "version": "v1", "kind": "Pod"},
    "resource": {"group": "", "version": "v1", "resource": "pods"},
    "namespace": "admission-tap-demo",
    "operation": "CREATE",
    "userInfo": {"username": "kubernetes-admin", "groups": ["system:authenticated"]},
    "object": { "...": "the full Pod" },
    "dryRun": false
  }
}
```

The webhook replies with the same envelope and a `response`:

```json
{
  "apiVersion": "admission.k8s.io/v1",
  "kind": "AdmissionReview",
  "response": {
    "uid": "705ab4f5-...",
    "allowed": true,
    "patchType": "JSONPatch",
    "patch": "W3sib3AiOiJhZGQiLC4uLg==",
    "warnings": ["admission-tap: added label admission-tap/mutated=true"]
  }
}
```

A few rules:

- `uid` must match the request's `uid`, or the API server rejects the response.
- `patch` is a base64-encoded [JSON Patch](https://datatracker.ietf.org/doc/html/rfc6902). Only mutating webhooks may return one.
- To deny, set `allowed: false` and put the reason in `result.message`. kubectl shows it to the user.
- `warnings` show up in kubectl as `Warning: ...` lines.

The types in `admission.go` mirror `admission.k8s.io/v1` by hand, so the repo has no dependencies and you can see the wire format directly.

## TLS and caBundle

The API server only calls webhooks over HTTPS, and it verifies the certificate. It reaches this server through the Service at `admission-tap.admission-tap.svc`, so:

1. The server certificate must have that name in its Subject Alternative Names.
2. Each webhook's `clientConfig.caBundle` must contain the CA that signed it.

`hack/gen-certs.sh` creates a self-signed CA and server certificate in `certs/`. `make deploy` stores the certificate in the `admission-tap-tls` Secret and inlines the CA as `caBundle`. In production, use cert-manager instead.

The server listens on two ports. 8443 serves the webhooks over TLS, with no client authentication. 8080 serves the query API over plain HTTP, protected by a bearer token from the `admission-tap-api-token` Secret (mounted at `/etc/webhook/api-token`). The API is never exposed on the webhook port.

## The three webhook configurations

| Configuration | Webhook | Rules | Scope | failurePolicy | Timeout |
|---|---|---|---|---|---|
| `MutatingWebhookConfiguration/admission-tap` | `mutate-pods` → `/mutate` | Pod CREATE | namespaces labelled `admission-tap=enabled` | `Fail` | 5s |
| `ValidatingWebhookConfiguration/admission-tap` | `validate-pods` → `/validate` | Pod CREATE, UPDATE | namespaces labelled `admission-tap=enabled` | `Fail` | 5s |
| `ValidatingWebhookConfiguration/admission-tap-capture` | `capture-all` → `/capture` | everything | all namespaces except `admission-tap`, plus cluster-scoped objects | `Ignore` | 2s |

The first two are in `deploy/20-webhooks.yaml`, the capture one is in `deploy/30-capture.yaml`.

The fields worth knowing:

- `rules`: which groups, versions, resources and operations are sent. In the capture webhook, `resources: ["*/*"]` means all resources and their subresources (like `pods/exec`), `operations: ["*"]` includes `CONNECT` (exec, attach, port-forward), and `scope: "*"` covers both namespaced and cluster-scoped objects.
- `namespaceSelector`: limits which namespaces are intercepted. The Pod webhooks use it as a safety net, so a broken webhook can't block `kube-system` or its own namespace. The capture webhook excludes `admission-tap` by its automatic `kubernetes.io/metadata.name` label, so the server can always restart itself.
- `failurePolicy`: what happens when the webhook is down or errors. `Fail` rejects the request. `Ignore` lets it through. Capture uses `Ignore` with a 2s timeout because it sits in front of every write in the cluster.
- `sideEffects: None`: the webhook changes nothing outside the object. This is required for `kubectl --dry-run=server` to call it.
- `reinvocationPolicy: Never`: the mutating webhook isn't called again if a later mutating webhook changes the object.

There are no `matchConditions`. As a result, a Pod created in the demo namespace is recorded three times: by mutate, validate and capture. Each record has a `webhook` field so you can tell them apart.
