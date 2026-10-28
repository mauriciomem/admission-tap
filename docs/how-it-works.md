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
    "namespace": "admission-tap-mutate-validate-demo",
    "operation": "CREATE",
    "userInfo": {"username": "kubernetes-admin", "groups": ["kubeadm:cluster-admins", "system:authenticated"]},
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
- To deny, set `allowed: false` and put the reason in `status.message`. kubectl shows it to the user. The key is `status`, not `result`; with any other key kubectl says "denied the request without explanation".
- `warnings` show up in kubectl as `Warning: ...` lines.

A denial looks like this:

```json
{
  "apiVersion": "admission.k8s.io/v1",
  "kind": "AdmissionReview",
  "response": {
    "uid": "705ab4f5-...",
    "allowed": false,
    "status": {
      "code": 403,
      "reason": "Forbidden",
      "message": "admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)"
    }
  }
}
```

The types in `admission.go` mirror `admission.k8s.io/v1` by hand, so the repo has no dependencies and you can see the wire format directly.


## TLS and caBundle

The API server only calls webhooks over HTTPS, and it verifies the certificate. It reaches this server through the Service at `admission-tap.admission-tap.svc`, so:

1. The server certificate must have that name in its Subject Alternative Names.
2. Each webhook's `clientConfig.caBundle` must contain the CA that signed it.

`scripts/gen-certs.sh` creates a self-signed CA and a server certificate in `certs/`. `scripts/deploy.sh` stores the certificate in the `admission-tap-tls` Secret and inlines the CA as `caBundle`. In production, use cert-manager instead.


## Two ports

The server listens on two ports:

- 8443 serves the webhooks over TLS, with no client authentication.
- 8080 serves the query API over plain HTTP. It's protected by a bearer token from the `admission-tap-api-token` Secret, mounted at `/etc/webhook/api-token`.

The API is never exposed on the webhook port.


## The three webhook configurations

The mutate and validate webhooks are in `deploy/20-webhooks.yaml`. The capture webhook is in `deploy/30-capture.yaml`. Each one has its own namespace label, and `scripts/deploy.sh` can leave either file out (see [Deploying](deploying.md#choosing-which-webhooks-to-register)).

### `MutatingWebhookConfiguration/admission-tap`

- Webhook `mutate-pods.admission-tap.dev`, path `/mutate`.
- Pod CREATE, in namespaces labelled `admission-tap=enabled`.
- `failurePolicy: Fail`, 5s timeout.

Adds the label `admission-tap/mutated=true` to new Pods. See [The mutate webhook](mutate-validate.md#the-mutate-webhook).

### `ValidatingWebhookConfiguration/admission-tap`

- Webhook `validate-pods.admission-tap.dev`, path `/validate`.
- Pod CREATE and UPDATE, in namespaces labelled `admission-tap=enabled`.
- `failurePolicy: Fail`, 5s timeout.

Rejects Pods whose images are untagged or use `:latest`. See [The validate webhook](mutate-validate.md#the-validate-webhook).

### `ValidatingWebhookConfiguration/admission-tap-capture`

- Webhook `capture-all.admission-tap.dev`, path `/capture`.
- Every namespaced resource and subresource, every operation, in namespaces labelled `admission-tap/capture=enabled`.
- Plus the Namespace objects with that label.
- Never the `admission-tap` namespace.
- `failurePolicy: Ignore`, 2s timeout.

Records every write and always allows it. See [Capture](capture.md#what-capture-does).


## The fields worth knowing

`rules` decide which groups, versions, resources and operations are sent. The capture webhook has two rules:

- `resources: ["*/*"]` with `scope: Namespaced` covers all namespaced resources and their subresources, like `pods/exec`. `operations: ["*"]` includes CONNECT, used by exec, attach and port-forward.
- `resources: ["namespaces"]` with `scope: Cluster` adds the Namespace objects themselves.

`namespaceSelector` limits which namespaces are intercepted:

- The Pod webhooks use it as a safety net, so a broken webhook can't block `kube-system` or its own namespace.
- The capture webhook matches `admission-tap/capture=enabled`. It also excludes `admission-tap` by its automatic `kubernetes.io/metadata.name` label, so the server can always restart itself.
- For a Namespace object, the selector is matched against the namespace's own labels.

`failurePolicy` decides what happens when the webhook is down or errors. `Fail` rejects the request. `Ignore` lets it through. Capture uses `Ignore` with a 2s timeout, so it never blocks or noticeably slows a captured namespace.

`sideEffects: None` says the webhook changes nothing outside the object. It's required for `kubectl --dry-run=server` to call the webhook.

`reinvocationPolicy: Never` means the mutating webhook isn't called again if a later mutating webhook changes the object.

There are no `matchConditions`. CEL expressions there can't see namespace labels. As a result, a Pod created in a namespace with both labels is recorded three times: by mutate, validate and capture. Each record has a `webhook` field so you can tell them apart (see [Records](records.md#what-a-record-is)).
