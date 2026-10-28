# Deploying

`scripts/deploy.sh` installs admission-tap into the cluster your current kubectl context points at. This page explains each of its steps and shows the plain commands, so you can run them by hand or adapt them.

Run all commands from the repository root.


## Running the script

`IMG` is required. It's the image the cluster will run:

```bash
IMG=ghcr.io/<you>/admission-tap:v0.1.0 scripts/deploy.sh
```

The script is safe to re-run. Every step creates or updates, and existing certificates and the existing token are kept.

When it finishes, it prints the next commands: label a namespace, read the token, port-forward to the API.


## Choosing which webhooks to register

Two variables decide which webhooks the script registers. Both default to `true`:

- `MUTATE_VALIDATE` registers the [mutate and validate](mutate-validate.md) webhooks (step 8).
- `CAPTURE` registers the [capture](capture.md) webhook (step 9).

Set one to `false` and the script doesn't register those webhooks. If they exist from an earlier run, it deletes them. For example, to run only mutate and validate:

```bash
CAPTURE=false IMG=$IMG scripts/deploy.sh
```

The server is deployed the same way whatever you choose, and records every call it receives. So with `CAPTURE=false`, mutate and validate keep working and their calls are still recorded. With `MUTATE_VALIDATE=false`, only capture calls arrive.

With both set to `false`, no webhooks are registered, the server receives no calls, and the script says so when it finishes.

To stop recording while keeping the webhooks, use the server flag `-record=false` instead (see [Records](records.md#turning-recording-off)).


## The steps

The namespace is always `admission-tap`. The certificate and every manifest refer to it by name.

### 1. Generate the CA and server certificate

The API server only calls webhooks over HTTPS, and it checks the certificate. This creates a self-signed CA and a server certificate for `admission-tap.admission-tap.svc`:

```bash
scripts/gen-certs.sh
```

It writes `certs/ca.crt`, `certs/ca.key`, `certs/tls.crt` and `certs/tls.key`. If `certs/` already has a certificate and a CA, it does nothing. Delete `certs/` to start over.

### 2. Generate the API token

A random 64-character hex string, readable only by you:

```bash
(umask 077 && openssl rand -hex 32 > certs/api-token)
```

The script skips this if `certs/api-token` already exists, so re-running doesn't change the token.

### 3. Create the namespace

```bash
kubectl apply -f deploy/00-namespace.yaml
```

The namespace enforces the `restricted` Pod Security Standard.

### 4. Create the TLS Secret

`kubectl create` fails if the Secret already exists. To create it or update it, the script renders the Secret locally and pipes it to `kubectl apply`:

```bash
kubectl -n admission-tap create secret tls admission-tap-tls \
  --cert=certs/tls.crt --key=certs/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -
```

`--dry-run=client -o yaml` prints the Secret instead of sending it. `kubectl apply -f -` reads it from stdin.

### 5. Create the API token Secret

Same pattern. The server reads the token from the key `token`:

```bash
kubectl -n admission-tap create secret generic admission-tap-api-token \
  --from-file=token=certs/api-token \
  --dry-run=client -o yaml | kubectl apply -f -
```

### 6. Deploy the server

`deploy/10-server.yaml` holds the ServiceAccount, the Deployment and the Service. It has two placeholders: `__IMAGE__` and `__SECRETS_CHECKSUM__`. It sets no server args, so every flag has its default and recording is on.

The server reads the certificate and the token only at startup, and updating a Secret doesn't restart Pods. So the script hashes the three files and stores the result in a Pod template annotation, `checksum/secrets`. If any file changed, the annotation changes and Kubernetes rolls the Pod. If none did, the Deployment is unchanged and nothing restarts.

```bash
SECRETS_CHECKSUM=$(cat certs/tls.crt certs/tls.key certs/api-token \
  | openssl dgst -sha256 -r | cut -d' ' -f1)
```

(`openssl` is already required, and behaves the same on Linux and macOS, unlike `sha256sum` and `shasum`.)

Replace the placeholders with `sed` and apply the result:

```bash
sed -e "s|__IMAGE__|$IMG|" -e "s|__SECRETS_CHECKSUM__|$SECRETS_CHECKSUM|" \
  deploy/10-server.yaml \
  | kubectl apply -f -
```

The Service exposes port 443 for the webhooks and port 80 for the query API.

### 7. Wait for the server

```bash
kubectl -n admission-tap rollout status deploy/admission-tap --timeout=120s
kubectl -n admission-tap get pods -l app=admission-tap
```

On an update, `rollout status` returns when the new Pod is Ready, but the old Pod is still terminating: it keeps serving for `-shutdown-delay` (5 seconds by default). Run the second command until only one Pod is listed, so that `kubectl logs` and `kubectl port-forward` reach the new one. The script does this for you, waiting up to 30 seconds.

This matters. The mutate and validate webhooks use `failurePolicy: Fail`. If they were registered before the server is ready, Pod creation in labelled namespaces would fail.

### 8. Register the mutate and validate webhooks

The webhook configurations need the CA, base64-encoded on a single line, in `caBundle`. Encode it once:

```bash
CA_BUNDLE=$(base64 < certs/ca.crt | tr -d '\n')
```

Then fill in the placeholder and apply:

```bash
sed "s|__CA_BUNDLE__|$CA_BUNDLE|" deploy/20-webhooks.yaml | kubectl apply -f -
```

This registers the mutate and validate webhooks for namespaces labelled `admission-tap=enabled`.

With `MUTATE_VALIDATE=false`, the script deletes them instead:

```bash
kubectl delete mutatingwebhookconfiguration admission-tap --ignore-not-found
kubectl delete validatingwebhookconfiguration admission-tap --ignore-not-found
```

Leaving them behind would keep Pod creation in labelled namespaces dependent on the server.

### 9. Register the capture webhook

```bash
sed "s|__CA_BUNDLE__|$CA_BUNDLE|" deploy/30-capture.yaml | kubectl apply -f -
```

With `CAPTURE=false`, the script deletes it instead:

```bash
kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
```

Nothing is intercepted yet. Label a namespace to opt it in, as described in [Capture](capture.md#turning-capture-on-and-off) and [Mutate and validate](mutate-validate.md#turning-them-on-and-off).


## Changing things after deploying

The server reads its certificate and token once, at startup. When `certs/tls.crt`, `certs/tls.key` or `certs/api-token` change, re-running the script restarts the server automatically (see step 6).

When you rotate the certificates (delete `certs/` and re-run the script), the new server certificate and the webhooks' `caBundle` are briefly out of sync, between step 6 and step 8. For a few seconds, Pod writes in namespaces with the mutate and validate webhooks (`failurePolicy: Fail`) can fail. Capture just skips those calls (`failurePolicy: Ignore`).

To rotate the API token, delete `certs/api-token` and re-run the script.

If you edit the Secrets by hand instead, the script can't see the change. Restart the server yourself:

```bash
kubectl -n admission-tap rollout restart deploy/admission-tap
```

Rebuilding the image under the same tag does not restart anything. The Deployment uses `imagePullPolicy: IfNotPresent`, so prefer a new tag and re-run the script.

To change server flags, add `args` to the container in `deploy/10-server.yaml` and re-run the script. See [Configuration](configuration.md#setting-flags).


## Undeploying

```bash
scripts/undeploy.sh
```

It runs these commands, in this order:

```bash
kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
kubectl delete validatingwebhookconfiguration admission-tap --ignore-not-found
kubectl delete mutatingwebhookconfiguration admission-tap --ignore-not-found
kubectl delete namespace admission-tap-capture-demo admission-tap-mutate-validate-demo admission-tap --ignore-not-found
```

The webhook configurations go first, so the API server stops calling a server that's about to disappear.

The two example namespaces, from `examples/capture/` and `examples/mutate-validate/`, are deleted too. Labels on your own namespaces are left as they are. They do nothing once the webhooks are gone, but you can remove them:

```bash
kubectl label namespace <ns> admission-tap/capture-   # capture
kubectl label namespace <ns> admission-tap-           # mutate and validate
```

`certs/` is kept, so a later deploy reuses the same CA and token. Delete it with `rm -rf certs`.


## The Makefile

The Makefile only builds and tests. It never talks to the cluster.

Targets:

- `make help` lists targets and variables. It's the default.
- `make test` runs `go vet` and `go test -race`.
- `make build` builds a local binary at `bin/admission-tap`.
- `make image` builds the container image.
- `make push` pushes it.

Variables:

- `IMG` is the image reference. Default: `admission-tap:dev`.
- `PLATFORM` is the target platform. Default: `linux/amd64`.
- `CONTAINER` is the tool to use. Default: `docker` if installed, otherwise `podman`.

For example, to build for arm64 nodes with podman:

```bash
make image push IMG=$IMG PLATFORM=linux/arm64 CONTAINER=podman
```
