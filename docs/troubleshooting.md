# Troubleshooting

The API server logs webhook failures too. On kubeadm clusters:

```bash
kubectl -n kube-system logs kube-apiserver-<node> | grep admission-tap
```


## Certificate errors

### `x509: certificate signed by unknown authority`

The `caBundle` in the webhook configurations doesn't match the CA that signed the server certificate.

Start over with fresh certificates:

```bash
scripts/undeploy.sh
rm -rf certs
IMG=$IMG scripts/deploy.sh
```

Note that `rm -rf certs` also deletes the API token, so a new one is generated.

### `x509: certificate is valid for ..., not admission-tap.admission-tap.svc`

The certificate doesn't include the Service name. Check which names it has:

```bash
openssl x509 -in certs/tls.crt -noout -ext subjectAltName
```

Then regenerate the certificates as above.


## The webhook can't be reached

### `connection refused` or `no endpoints available`

The server Pod isn't ready, the image is wrong, or the Service doesn't match the Pod. Look at the Pod and its endpoints:

```bash
kubectl -n admission-tap get pods,endpoints
kubectl -n admission-tap describe pod
```

### `context deadline exceeded`

The API server can't reach the Pod network. Check the CNI, firewalls between the control plane and the nodes, and any NetworkPolicies in `admission-tap`.

### Pod stuck in `ImagePullBackOff`

The image wasn't pushed, is private, was built for the wrong architecture, or wasn't loaded into kind. Check `IMG`, the registry's visibility, and `PLATFORM`.


## Capture

### No `capture` records

Check, in order:

1. The namespace has the label. `kubectl get namespace <ns> --show-labels` should list `admission-tap/capture=enabled`.
2. The label was only just added. Capture normally starts almost immediately; if it doesn't, check the label with `kubectl get ns <ns> --show-labels`.
3. The object is in that namespace. Cluster-scoped objects other than Namespaces are never captured.
4. The capture webhook exists. If you deployed with `CAPTURE=false`, it doesn't:

   ```bash
   kubectl get validatingwebhookconfiguration admission-tap-capture
   ```

5. `captureEnabled` is `true` in `/api/stats`.

Capture uses `failurePolicy: Ignore`, so failed calls don't show up as errors anywhere in kubectl. Look in the API server logs.

### Records still arrive after removing the label

Capture normally stops almost immediately after you remove the label. If records still arrive, check that the label is really gone (`kubectl get ns <ns> --show-labels`). Records for calls that were already in flight can also arrive just after.

### Your record is gone

Older records are evicted when the store is full, and everything is lost when the Pod restarts.

Filter more narrowly and query sooner, or raise the store limits and the memory limit (see [Configuration](configuration.md)).


## Query API

### `401 missing or invalid bearer token`

The `Authorization` header is missing, or the token is out of date. Read it again from the Secret and send it with `-H "Authorization: Bearer $TOKEN"`:

```bash
export TOKEN=$(kubectl -n admission-tap get secret admission-tap-api-token \
  -o jsonpath='{.data.token}' | base64 -d)
```

If you changed the Secret recently, the server may still have the old token. It only reads it at startup. Re-running `scripts/deploy.sh` restarts it automatically when `certs/api-token` has changed. If you edited the Secret by hand, restart the server yourself:

```bash
kubectl -n admission-tap rollout restart deploy/admission-tap
```

### The Pod crashes with `query API token` in the logs

The token Secret is missing or empty. Re-running `scripts/deploy.sh` recreates it from `certs/api-token`. Delete that file first to get a new token.

### `kubectl port-forward` fails

The server isn't running, or port 8080 is already in use on your machine. Check the Pod with `kubectl -n admission-tap get pods`, or forward another local port:

```bash
kubectl -n admission-tap port-forward svc/admission-tap 9090:80
```


## Emergency removal

If a webhook is blocking things, delete its configuration. The API server stops calling it right away.

This always works, because webhook configurations are never sent to webhooks:

```bash
kubectl delete mutatingwebhookconfiguration admission-tap
kubectl delete validatingwebhookconfiguration admission-tap admission-tap-capture
```

Then fix the server and re-run `scripts/deploy.sh`, or remove everything with `scripts/undeploy.sh`.
