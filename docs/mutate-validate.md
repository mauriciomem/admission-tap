# Mutate and validate

admission-tap ships two small example webhooks for Pods. One changes Pods as they're created, the other rejects Pods that break a rule. They show the two things an admission webhook can do besides watching: patch an object, and say no.

The server records their calls just like capture's. The format of the records, redaction and where they're stored are described in [Records](records.md).


## The mutate webhook

`MutatingWebhookConfiguration/admission-tap`, in `deploy/20-webhooks.yaml`, has one webhook, `mutate-pods.admission-tap.dev`. The API server calls it on `/mutate` for every Pod CREATE in a namespace labelled `admission-tap=enabled`.

It adds the label `admission-tap/mutated=true` to the Pod. It does this by answering with a base64-encoded [JSON Patch](https://datatracker.ietf.org/doc/html/rfc6902) and a warning, which kubectl prints:

```
Warning: admission-tap: added label admission-tap/mutated=true
```

The patch depends on whether the Pod already has labels:

- No labels: `[{"op":"add","path":"/metadata/labels","value":{"admission-tap/mutated":"true"}}]`
- Some labels: `[{"op":"add","path":"/metadata/labels/admission-tap~1mutated","value":"true"}]`

The `~1` is how JSON Pointer writes a `/` inside a key.

Records have `"webhook": "mutate"`.


## The validate webhook

`ValidatingWebhookConfiguration/admission-tap`, in the same file, has one webhook, `validate-pods.admission-tap.dev`. The API server calls it on `/validate` for every Pod CREATE and UPDATE in a namespace labelled `admission-tap=enabled`.

It rejects a Pod if any of its containers or init containers uses an image with no tag, or with the tag `latest`:

- `nginx` and `nginx:latest` are rejected.
- `nginx:1.27` is allowed.
- An image pinned by digest, like `nginx@sha256:...`, is always allowed.
- A registry port isn't mistaken for a tag: `localhost:5000/nginx` is rejected, `localhost:5000/nginx:1.27` is allowed.

The denial lists every offending container as `name (image)`, and comes back with code 403, reason `Forbidden`. kubectl shows it as an error.

Records have `"webhook": "validate"`.


## Turning them on and off

### For one namespace

Neither webhook does anything until you label a namespace:

```bash
kubectl label namespace <ns> admission-tap=enabled
```

To stop, remove the label. The trailing `-` means "remove":

```bash
kubectl label namespace <ns> admission-tap-
```

The one label turns on both webhooks. There's no label for just one of them.

### For the whole cluster

To remove both webhooks altogether, deploy with `MUTATE_VALIDATE=false`:

```bash
MUTATE_VALIDATE=false IMG=$IMG scripts/deploy.sh
```

The script deletes `MutatingWebhookConfiguration/admission-tap` and `ValidatingWebhookConfiguration/admission-tap` if they exist, and doesn't register them. [Capture](capture.md) keeps working. Deploy again without `MUTATE_VALIDATE=false` to bring them back.

[Deploying](deploying.md#choosing-which-webhooks-to-register) covers both script variables.


## How they differ from capture

- They only see Pods, and only CREATE (mutate) or CREATE and UPDATE (validate). Capture sees every write to every namespaced resource.
- They act on the Pod: mutate changes it, validate can reject it. Capture only records and always allows.
- They use their own label, `admission-tap=enabled`. Capture uses `admission-tap/capture=enabled`. A namespace can have either, both, or neither.
- They use `failurePolicy: Fail`, with a 5s timeout. If the server is down or unreachable, Pod creates and updates in labelled namespaces fail. Capture uses `Ignore`, so writes go through unrecorded.

Because of `Fail`, the deploy script registers them only once the server is ready. If a broken server blocks Pods, see [Emergency removal](troubleshooting.md#emergency-removal).


## Their calls are recorded

The server records every call it receives, from any of its webhooks. Mutate and validate calls are recorded whether or not capture is on, and whether or not the namespace also has the capture label.

Filter them with `webhook=mutate` or `webhook=validate` (see [Query API](query-api.md#filters)).

In a namespace with both labels, a Pod creation is recorded three times: by mutate, validate and capture.


## A mutate record

This is the mutate record for `kubectl apply -f examples/mutate-validate/pod-allowed.yaml`, trimmed. The `http` block has the same shape as in [a capture record](capture.md#a-capture-record):

```json
{
  "id": 12,
  "time": "2026-10-27T09:20:31.904Z",
  "durationMs": 0.21,
  "webhook": "mutate",
  "status": 200,
  "http": {"method": "POST", "path": "/mutate", "rawQuery": "timeout=5s", "...": "..."},
  "uid": "1f6a9d40-...",
  "operation": "CREATE",
  "kind": {"group": "", "version": "v1", "kind": "Pod"},
  "resource": {"group": "", "version": "v1", "resource": "pods"},
  "namespace": "admission-tap-mutate-validate-demo",
  "name": "web-ok",
  "username": "kubernetes-admin",
  "groups": ["kubeadm:cluster-admins", "system:authenticated"],
  "dryRun": false,
  "allowed": true,
  "request": {
    "kind": "AdmissionReview",
    "apiVersion": "admission.k8s.io/v1",
    "request": {"uid": "1f6a9d40-...", "operation": "CREATE", "object": {"kind": "Pod", "...": "..."}, "...": "..."}
  },
  "response": {
    "apiVersion": "admission.k8s.io/v1",
    "kind": "AdmissionReview",
    "response": {
      "uid": "1f6a9d40-...",
      "allowed": true,
      "patch": "W3sib3AiOiJhZGQiLCJwYXRoIjoiL21ldGFkYXRhL2xhYmVscyIsInZhbHVlIjp7ImFkbWlzc2lvbi10YXAvbXV0YXRlZCI6InRydWUifX1d",
      "patchType": "JSONPatch",
      "warnings": ["admission-tap: added label admission-tap/mutated=true"]
    }
  }
}
```

The patch is base64-encoded. To read it, extract it with jq and decode it with `base64 -d`:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/records?webhook=mutate&name=web-ok&limit=1' \
  | jq -r '.items[0].response.response.patch' | base64 -d
```

```
[{"op":"add","path":"/metadata/labels","value":{"admission-tap/mutated":"true"}}]
```


## A validate record

This is the validate record for `kubectl apply -f examples/mutate-validate/pod-denied.yaml`, trimmed:

```json
{
  "id": 15,
  "time": "2026-10-27T09:21:02.117Z",
  "durationMs": 0.18,
  "webhook": "validate",
  "status": 200,
  "http": {"method": "POST", "path": "/validate", "rawQuery": "timeout=5s", "...": "..."},
  "uid": "8c2e51b7-...",
  "operation": "CREATE",
  "kind": {"group": "", "version": "v1", "kind": "Pod"},
  "resource": {"group": "", "version": "v1", "resource": "pods"},
  "namespace": "admission-tap-mutate-validate-demo",
  "name": "web-latest",
  "username": "kubernetes-admin",
  "groups": ["kubeadm:cluster-admins", "system:authenticated"],
  "dryRun": false,
  "allowed": false,
  "request": {
    "kind": "AdmissionReview",
    "apiVersion": "admission.k8s.io/v1",
    "request": {"uid": "8c2e51b7-...", "operation": "CREATE", "object": {"kind": "Pod", "...": "..."}, "...": "..."}
  },
  "response": {
    "apiVersion": "admission.k8s.io/v1",
    "kind": "AdmissionReview",
    "response": {
      "uid": "8c2e51b7-...",
      "allowed": false,
      "status": {
        "code": 403,
        "reason": "Forbidden",
        "message": "admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)"
      }
    }
  }
}
```

`status` is still 200: the webhook answered fine, and the answer was no. The Pod in `request.request.object` already has the `admission-tap/mutated` label, because mutation ran first.

To list every denial:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/records?webhook=validate&allowed=false&summary=true'
```


## Walkthrough

This uses the files in `examples/mutate-validate/`. It assumes admission-tap is deployed with mutate and validate on, which is the default.

Create the namespace. It has only the `admission-tap=enabled` label, so capture stays out of the way and you only see these two webhooks:

```bash
kubectl apply -f examples/mutate-validate/namespace.yaml
```

Create a Pod with a pinned tag. It's allowed, and mutate adds the label:

```bash
kubectl apply -f examples/mutate-validate/pod-allowed.yaml
```

```
Warning: admission-tap: added label admission-tap/mutated=true
pod/web-ok created
```

Check the label:

```bash
kubectl -n admission-tap-mutate-validate-demo get pod web-ok --show-labels
```

Create a Pod that uses `nginx:latest`. It's rejected:

```bash
kubectl apply -f examples/mutate-validate/pod-denied.yaml
```

```
Warning: admission-tap: added label admission-tap/mutated=true
Error from server (Forbidden): error when creating "examples/mutate-validate/pod-denied.yaml": admission webhook "validate-pods.admission-tap.dev" denied the request: admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)
```

The error is one long line; scroll the block if it's cut off.

List what the server recorded for this namespace. You should see a mutate and a validate record for each Pod, the last one with `"allowed": false`:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/records?namespace=admission-tap-mutate-validate-demo&summary=true'
```

To clean up, delete the namespace, or run `scripts/undeploy.sh`, which deletes it too:

```bash
kubectl delete namespace admission-tap-mutate-validate-demo
```


## Things to know

### A denied Pod still gets the warning

Mutating webhooks run before validating ones. So mutate has already patched the Pod, and sent its warning, by the time validate rejects it.

### UPDATE is validated, not mutated

Mutate only handles CREATE. Validate handles CREATE and UPDATE, so changing a running Pod's image to `:latest` is rejected too.

### Subresources are ignored

The rules match `pods`, not subresources like `pods/exec` or `pods/status`, so those calls never reach these webhooks. The code also allows any subresource call it does get.

### No reinvocation

`reinvocationPolicy: Never` means mutate isn't called again if a later mutating webhook changes the Pod.

### Pods created by controllers

The webhooks see Pods, not Deployments. A Deployment that uses `nginx:latest` is created fine, but its ReplicaSet can't create any Pods. The denial shows up as a `FailedCreate` event on the ReplicaSet:

```bash
kubectl -n <ns> describe replicaset
```


## Things worth trying

In the walkthrough namespace:

```bash
# An UPDATE: validate rejects switching the image to :latest
kubectl -n admission-tap-mutate-validate-demo set image pod/web-ok web=nginx:latest

# No tag at all, as a server-side dry run: rejected, and nothing is created
kubectl -n admission-tap-mutate-validate-demo run dry --image=nginx --dry-run=server

# A Pod that already has labels: decode its patch and look for ~1
kubectl -n admission-tap-mutate-validate-demo run tagged --image=nginx:1.27
```

`kubectl run` adds a `run=<name>` label, so the last command gets the second kind of patch. Decode it as shown in [A mutate record](#a-mutate-record), with `name=tagged`.
