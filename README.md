# admission-tap

admission-tap is a Kubernetes admission webhook server that records the AdmissionReviews the API server sends it, together with its replies. You can read the records back over a small, token-protected query API, or follow them on stdout as JSON lines.

It's written in Go with only the standard library.


## What's inside

The server has three webhooks, in two features. Each feature is turned on per namespace by its own label.

Capture, on `/capture`, records every write in a namespace labelled `admission-tap/capture=enabled`: any resource, subresource or operation, including `kubectl exec`. It always allows, so it never gets in the way. You can turn it off for the whole cluster and keep the other two.

Mutate and validate, on `/mutate` and `/validate`, are two example webhooks for Pods in namespaces labelled `admission-tap=enabled`. Mutate adds the label `admission-tap/mutated=true` to new Pods. Validate rejects Pods whose images use `:latest` or have no tag.

The server records every call it receives, from any of the three. So mutate and validate calls are recorded even when capture is off.


## Prerequisites

- A Kubernetes cluster, 1.28 or newer.
- `kubectl` with cluster-admin rights.
- `docker` or `podman`, `openssl` and `make`.
- `jq` is handy but optional.


## Quick start

### 1. Build and push the image

Push it somewhere your nodes can pull from:

```bash
export IMG=ghcr.io/<you>/admission-tap:v0.1.0
make image push IMG=$IMG
```

The image is built for `linux/amd64`. For arm64 nodes, add `PLATFORM=linux/arm64`.

On kind, skip the push and load the image instead: `kind load docker-image $IMG`.

### 2. Deploy

```bash
IMG=$IMG scripts/deploy.sh
```

The script creates certificates and an API token in `certs/`, deploys the server, waits for it, and then registers all three webhooks. It's safe to re-run.

To leave out capture, add `CAPTURE=false`. To leave out mutate and validate, add `MUTATE_VALIDATE=false`. [docs/deploying.md](docs/deploying.md) walks through each step with the plain kubectl commands.

Nothing is intercepted until you label a namespace. The next two steps are independent: try either or both.

### 3a. Capture a namespace

Follow the server's records in a separate terminal:

```bash
kubectl -n admission-tap logs deploy/admission-tap -f
```

Create the example namespace, which has the capture label, and write a few things into it:

```bash
kubectl apply -f examples/capture/namespace.yaml
kubectl apply -f examples/capture/configmap.yaml
kubectl apply -f examples/capture/deployment.yaml
```

Each write shows up as a record. The Deployment brings many more, written by the controllers on your behalf.

To capture your own namespace, label it. To stop, remove the label (the trailing `-` removes it):

```bash
kubectl label namespace <ns> admission-tap/capture=enabled
kubectl label namespace <ns> admission-tap/capture-
```

More in [docs/capture.md](docs/capture.md).

### 3b. Mutate and validate Pods

Create the example namespace, which has the `admission-tap=enabled` label, and a Pod with a pinned tag:

```bash
kubectl apply -f examples/mutate-validate/namespace.yaml
kubectl apply -f examples/mutate-validate/pod-allowed.yaml
```

The Pod is created, with a warning that a label was added:

```
Warning: admission-tap: added label admission-tap/mutated=true
pod/web-ok created
```

Now a Pod that uses `nginx:latest`:

```bash
kubectl apply -f examples/mutate-validate/pod-denied.yaml
```

It's rejected:

```
Error from server (Forbidden): error when creating "examples/mutate-validate/pod-denied.yaml": admission webhook "validate-pods.admission-tap.dev" denied the request: admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)
```

That is one long line; scroll the block if it is cut off. It still gets the warning, because mutation runs before validation.

More in [docs/mutate-validate.md](docs/mutate-validate.md).

### 4. Read the records

The logs show every record as one JSON line:

```bash
kubectl -n admission-tap logs deploy/admission-tap
```

For the query API, read the token from its Secret. Secret values are base64-encoded, so decode it:

```bash
export TOKEN=$(kubectl -n admission-tap get secret admission-tap-api-token \
  -o jsonpath='{.data.token}' | base64 -d)
```

In another terminal, forward local port 8080 to the Service:

```bash
kubectl -n admission-tap port-forward svc/admission-tap 8080:80
```

Then ask for the latest records:

```bash
curl -s -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/records?summary=true'
```

Filter by webhook, for example what capture saw, or what validate denied:

```bash
curl -s -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/records?webhook=capture&summary=true'
curl -s -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/records?webhook=validate&allowed=false&summary=true'
```

The record format is in [docs/records.md](docs/records.md), and every endpoint and filter in [docs/query-api.md](docs/query-api.md).


## Cleanup

```bash
scripts/undeploy.sh
```

The script deletes the webhooks first, then the two example namespaces and `admission-tap`. `certs/` holds the CA and the API token, and is kept so a later deploy reuses them.

To also delete the local CA and API token: `rm -rf certs`


## Security notes

admission-tap is meant for development and test clusters, not production.

Records hold whatever is written in a captured namespace, and every Pod sent to mutate and validate: Pod specs, env vars, ConfigMaps, exec commands. Only core/v1 Secrets are redacted.

Anyone with the API token, or who can read its Secret in `admission-tap`, can read those records.

Anyone who can label namespaces can turn capture, mutate and validate on or off for a namespace.

The webhook port (8443) has no client authentication. Anything that can reach it can post fake reviews into the store.

Every write in a captured namespace gets one extra webhook call. It times out after 2s, and failures are ignored. Mutate and validate are different: if the server is down, Pod creates and updates in their namespaces fail.


## Documentation

- [Deploying](docs/deploying.md): what the deploy script does, step by step, choosing which webhooks to register, and the Makefile.
- [Capture](docs/capture.md): turning it on and off, what is and isn't captured, and a capture record.
- [Mutate and validate](docs/mutate-validate.md): what the example Pod webhooks do, turning them on and off, and their records.
- [Records](docs/records.md): the record format, redaction, the store and stdout.
- [Query API](docs/query-api.md): endpoints, filters and examples.
- [Configuration](docs/configuration.md): server flags.
- [How it works](docs/how-it-works.md): the admission flow, TLS, and the webhook configurations.
- [Troubleshooting](docs/troubleshooting.md): common errors and emergency removal.
