# admission-tap

A Kubernetes admission webhook that records every AdmissionReview the API server sends it, along with its reply, and serves them over a small query API. Records also go to stdout as JSON lines. It's written in Go using only the standard library.

It serves three webhook endpoints. `/mutate` and `/validate` are built-in examples of a mutating and a validating webhook:

- `/mutate` adds the label `admission-tap/mutated=true` to new Pods.
- `/validate` rejects Pods whose images use `:latest` or have no tag.
- `/capture` allows everything and receives every admission call in the cluster.

Mutate and validate only act on namespaces labelled `admission-tap=enabled`. Capture covers every namespace except `admission-tap`.

## Prerequisites

- A Kubernetes cluster, 1.28 or newer, and `kubectl` with cluster-admin rights.
- `docker` or `podman`, `openssl` and `make`. `jq` is handy but optional.

## Quick start

Build the image and push it somewhere your nodes can pull from:

```bash
export IMG=ghcr.io/<you>/admission-tap:v0.1.0
make image push IMG=$IMG
```

This builds for `linux/amd64` by default, with docker or podman, whichever is installed. Set `PLATFORM=linux/arm64` for arm64 nodes, or `CONTAINER=podman` to force podman. On kind, skip the push and run `kind load docker-image $IMG`.

Deploy it:

```bash
make deploy IMG=$IMG
```

This generates a self-signed CA, a server certificate and a random API token in `certs/`, creates the namespace, the TLS and token Secrets and the server, waits for it to be ready, and only then registers the webhooks. To run without cluster-wide capture, add `CAPTURE=false`. The server then runs with `-capture=false` and the `admission-tap-capture` webhook is not registered (or is deleted if it already exists).

### Try it

In one terminal, follow the logs:

```bash
make logs
```

In another, run the demo:

```bash
kubectl apply -f examples/namespace.yaml

kubectl apply -f examples/pod-allowed.yaml
# Warning: admission-tap: added label admission-tap/mutated=true
# pod/web-ok created

kubectl apply -f examples/pod-denied.yaml
# Warning: admission-tap: added label admission-tap/mutated=true
# Error from server (Forbidden): error when creating "examples/pod-denied.yaml":
#   admission webhook "validate-pods.admission-tap.dev" denied the request:
#   admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)
```

The denied Pod still got the warning. Mutation runs first, then validation rejects it. (`make demo` runs the same three commands.)

## Querying captures

Every call is written to stdout as one JSON line and kept in a bounded in-memory store. The store is served over plain HTTP on port 8080, exposed by the Service on port 80.

All `/api/*` endpoints need `Authorization: Bearer <token>`. `make token` prints the token from the cluster Secret. Export it once to keep commands short:

```bash
export TOKEN=$(make -s token)
```

From your laptop:

```bash
make api        # port-forwards svc/admission-tap to localhost:8080
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/stats
curl -s -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/captures?namespace=admission-tap-demo&summary=true'
curl -s -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/captures?allowed=false'
```

From inside the cluster, pass the token into the curl Pod:

```bash
kubectl run curl --rm -it --image=curlimages/curl:8.10.1 --restart=Never --env TOKEN=$TOKEN -- \
  sh -c 'curl -s -H "Authorization: Bearer $TOKEN" http://admission-tap.admission-tap.svc/api/stats'
```

`curl localhost:8080/` prints a short help text and needs no token. See [docs/capturing.md](docs/capturing.md) for all filters and the record format.

## Configuration

Server flags (set in `args` in `deploy/10-server.yaml`):

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8443` | Webhook HTTPS listen address |
| `-tls-cert` | `/etc/webhook/certs/tls.crt` | TLS certificate |
| `-tls-key` | `/etc/webhook/certs/tls.key` | TLS private key |
| `-api-addr` | `:8080` | Query API listen address (plain HTTP). Empty disables it |
| `-api-token-file` | `/etc/webhook/api-token/token` | Bearer token for `/api/*`. Empty disables auth (with a warning). A missing or empty file stops startup |
| `-capture` | `true` | Record calls to stdout and the store |
| `-capture-stdout` | `full` | What each capture writes to stdout: `full`, `summary` (no payloads) or `off`. The store always keeps full records |
| `-redact-secrets` | `true` | Redact Secret `data`/`stringData` and the kubectl last-applied annotation |
| `-capture-max-entries` | `1000` | Maximum records kept in memory |
| `-capture-max-bytes` | `33554432` (32 MiB) | Maximum total size of records kept in memory |

Make variables: `IMG` (default `admission-tap:dev`), `CAPTURE` (default `true`), `PLATFORM` (default `linux/amd64`) and `CONTAINER` (`docker` if installed, otherwise `podman`). Besides `deploy`, `logs`, `api` and `undeploy`, there's `make token` (print the in-cluster token) and `make api-token` (generate `certs/api-token` if it's missing; `deploy` does this for you).

Cluster-wide capture is noisy. Leases, events and Pod status updates arrive constantly, so the oldest records are evicted quickly. The container has a 256Mi memory limit. If you raise the store limits, raise that too.

## Security notes

admission-tap is intended for development and test clusters, not production.

- Captures contain every write in the cluster, including Pod specs, env vars, ConfigMaps, exec commands, and secrets held in anything other than a core/v1 Secret.
- Anyone with the API token, or who can read its Secret in `admission-tap`, can see all of that.
- The webhook port (8443) has no client authentication, so anything that can reach it can post fake reviews into the store.
- Every write in the cluster gets an extra webhook call (2s max, failures ignored).

## Cleanup

```bash
make undeploy
rm -rf certs bin
```

## More

- [How it works](docs/how-it-works.md): the admission flow, TLS, and the three webhook configurations.
- [Capturing](docs/capturing.md): what is and isn't captured, the record format, the API reference, and jq tips.
- [Troubleshooting](docs/troubleshooting.md): common errors and emergency removal.
