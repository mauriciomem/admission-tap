# admission-tap

admission-tap is a Kubernetes admission webhook that records the AdmissionReviews the API server sends it, together with its replies. You can read the records back over a small, token-protected query API, or follow them on stdout as JSON lines.

Capture is opt-in. Only namespaces labelled `admission-tap/capture=enabled` are recorded.

It also ships two example webhooks for Pods: one mutating, one validating. It's written in Go with only the standard library.


## What's inside

The server exposes three webhook endpoints:

- `/capture` allows everything and records it.
- `/mutate` adds the label `admission-tap/mutated=true` to new Pods.
- `/validate` rejects Pods whose images use `:latest` or have no tag.

Each one is turned on by its own namespace label:

- `admission-tap/capture=enabled` for capture.
- `admission-tap=enabled` for the mutate and validate examples.


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

The script creates certificates and an API token in `certs/`, deploys the server, waits for it, and then registers the webhooks. It's safe to re-run.

[docs/deploying.md](docs/deploying.md) walks through each step with the plain kubectl commands.

### 3. Follow the logs

In a separate terminal:

```bash
kubectl -n admission-tap logs deploy/admission-tap -f
```

### 4. Try the demo

The demo namespace has both labels, so all three webhooks see it:

```bash
kubectl apply -f examples/namespace.yaml
kubectl apply -f examples/pod-allowed.yaml
kubectl apply -f examples/pod-denied.yaml
```

The first Pod is created with a warning that a label was added.

The second Pod is rejected:

```
Error from server (Forbidden): error when creating "examples/pod-denied.yaml": admission webhook "validate-pods.admission-tap.dev" denied the request: admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)
```

That is one long line; scroll the block if it is cut off. It still gets the warning, because mutation runs before validation.

### 5. Capture your own namespace

Add the capture label to any namespace you want to record:

```bash
kubectl label namespace <ns> admission-tap/capture=enabled
```

To stop, remove the label (the trailing `-` removes it):

```bash
kubectl label namespace <ns> admission-tap/capture-
```

### 6. Query the API

Read the token from its Secret. Secret values are base64-encoded, so decode it:

```bash
export TOKEN=$(kubectl -n admission-tap get secret admission-tap-api-token \
  -o jsonpath='{.data.token}' | base64 -d)
```

In another terminal, forward local port 8080 to the Service:

```bash
kubectl -n admission-tap port-forward svc/admission-tap 8080:80
```

Then ask for the latest calls in the demo namespace:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/captures?namespace=admission-tap-demo&summary=true'
```

The full list of endpoints and filters is in [docs/query-api.md](docs/query-api.md).


## Cleanup

```bash
scripts/undeploy.sh
```

The script deletes the webhooks first, then the `admission-tap` and `admission-tap-demo` namespaces. `certs/` holds the CA and the API token, and is kept so a later deploy reuses them.

To also delete the local CA and API token: `rm -rf certs`


## Security notes

admission-tap is meant for development and test clusters, not production.

Records hold whatever is written in a captured namespace: Pod specs, env vars, ConfigMaps, exec commands. Only core/v1 Secrets are redacted.

Anyone with the API token, or who can read its Secret in `admission-tap`, can read those records.

Anyone who can label namespaces can turn capture on or off for a namespace.

The webhook port (8443) has no client authentication. Anything that can reach it can post fake reviews into the store.

Every write in a captured namespace gets one extra webhook call. It times out after 2s, and failures are ignored.


## Documentation

- [Deploying](docs/deploying.md): what the deploy script does, step by step, and the Makefile.
- [Capturing](docs/capturing.md): what is and isn't captured, redaction, and the record format.
- [Query API](docs/query-api.md): endpoints, filters and examples.
- [Configuration](docs/configuration.md): server flags.
- [How it works](docs/how-it-works.md): the admission flow, TLS, and the webhook configurations.
- [Troubleshooting](docs/troubleshooting.md): common errors and emergency removal.
