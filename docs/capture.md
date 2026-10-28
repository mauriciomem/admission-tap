# Capture

The capture webhook records every write in the namespaces you opt in, and always allows it. It's how you see what the API server actually sends to an admission webhook: who did what, to which object, with the full payload.

The format of the records, redaction and where they're stored are shared by all three webhooks. They're described in [Records](records.md).


## What capture does

The server serves capture on the path `/capture`. It's registered by `ValidatingWebhookConfiguration/admission-tap-capture`, in `deploy/30-capture.yaml`.

For each call, the server records the AdmissionReview it received and the answer it sent. The answer is always `allowed: true`, with no patch and no warning. Capture never changes or blocks anything.

Records have `"webhook": "capture"`.


## Turning capture on and off

### For one namespace

Nothing is captured until you label a namespace:

```bash
kubectl label namespace <ns> admission-tap/capture=enabled
```

To stop, remove the label. The trailing `-` means "remove":

```bash
kubectl label namespace <ns> admission-tap/capture-
```

Capture starts and stops almost immediately after you change the label.

The `admission-tap` namespace is never captured, even if labelled. That way the server can always restart itself.

### For the whole cluster

To remove the capture webhook altogether, deploy with `CAPTURE=false`:

```bash
CAPTURE=false IMG=$IMG scripts/deploy.sh
```

The script deletes `admission-tap-capture` if it exists, and doesn't register it. The labels on your namespaces stay, but do nothing.

The [mutate and validate webhooks](mutate-validate.md) keep working, and their calls are still recorded. Deploy again without `CAPTURE=false` to bring capture back.

[Deploying](deploying.md#choosing-which-webhooks-to-register) covers both script variables.


## How capture differs from mutate and validate

| | Capture | Mutate and validate |
|---|---|---|
| Namespace label | `admission-tap/capture=enabled` | `admission-tap=enabled` |
| What it sees | Every write to every namespaced resource | Pods only |
| What it does | Records and allows | Changes or rejects Pods |
| `failurePolicy` | `Ignore`, 2s timeout | `Fail`, 5s timeout |
| If the server is down | Writes go through, unrecorded | Pod creates and updates fail |

The two labels are independent. A namespace can have either, both, or neither.

Every call the server receives is recorded, whichever webhook sent it. So mutate and validate calls show up in the records too, without the capture label, with `webhook` set to `mutate` or `validate`.


## What is captured

In a labelled namespace, capture receives every write:

- All resources, in every API group, including custom resources.
- All subresources, such as `pods/exec`, `pods/status` and `deployments/scale`.
- All operations: CREATE, UPDATE, DELETE and CONNECT. CONNECT covers `kubectl exec`, `attach` and `port-forward`.

Changes to the labelled Namespace object itself are captured too.

Records follow the object's namespace, not the requester's. For example, when you create a Deployment in a labelled namespace, the ReplicaSet and Pods that the controllers create there are captured as well.

Failed and malformed calls are recorded too. See [Records](records.md#what-a-record-is).


## What isn't captured

Other namespaces, and cluster-scoped objects other than Namespaces: Nodes, ClusterRoles, CustomResourceDefinitions, PersistentVolumes and so on.

These are never sent to any webhook, by Kubernetes design:

- Reads: GET, LIST and WATCH. Admission only covers writes. To see reads, use the API server audit log.
- Webhook configurations and admission policies, and their bindings. This way a broken webhook can't stop you from fixing it.
- Requests rejected by authentication or authorization. They never reach admission.


## Things to know

### Labelling the Namespace object

For changes to the Namespace object itself, the API server looks at the new labels. So the update that adds the capture label is captured, and the one that removes it isn't.

### Capture's `allowed` is always true

Validating webhooks run in parallel. A capture record's `allowed` is capture's own answer, which is always `true`.

It doesn't tell you whether another webhook denied the request. If the namespace also has `admission-tap=enabled`, look at the `validate` record for that.

### Noise

Busy namespaces produce a steady stream of Events, status updates and controller writes. The store is bounded, so older records get evicted. Filter narrowly and query soon, or raise the limits (see [Configuration](configuration.md#recording)).

### `stringData` arrives as `data`

The API server converts a Secret's `stringData` to `data` before validating webhooks run, so captured Secrets only ever have `data`. Its values are redacted (see [Redaction](records.md#redaction)).


## A capture record

This is the record for `kubectl apply -f examples/capture/configmap.yaml`, trimmed. Values like ids, addresses and the user depend on your cluster:

```json
{
  "id": 7,
  "time": "2026-10-27T09:12:04.318Z",
  "durationMs": 0.12,
  "webhook": "capture",
  "status": 200,
  "http": {
    "method": "POST",
    "path": "/capture",
    "rawQuery": "timeout=2s",
    "remoteAddr": "10.244.0.1:43712",
    "headers": {
      "Content-Type": ["application/json"],
      "User-Agent": ["kube-apiserver-admission"],
      "...": []
    },
    "contentLength": 1384,
    "tls": {"version": "TLS1.3", "cipherSuite": "TLS_AES_128_GCM_SHA256", "serverName": "admission-tap.admission-tap.svc"}
  },
  "uid": "b3c0e7a2-...",
  "operation": "CREATE",
  "kind": {"group": "", "version": "v1", "kind": "ConfigMap"},
  "resource": {"group": "", "version": "v1", "resource": "configmaps"},
  "namespace": "admission-tap-capture-demo",
  "name": "demo-config",
  "username": "kubernetes-admin",
  "groups": ["kubeadm:cluster-admins", "system:authenticated"],
  "dryRun": false,
  "allowed": true,
  "request": {
    "kind": "AdmissionReview",
    "apiVersion": "admission.k8s.io/v1",
    "request": {
      "uid": "b3c0e7a2-...",
      "operation": "CREATE",
      "userInfo": {"username": "kubernetes-admin", "groups": ["kubeadm:cluster-admins", "system:authenticated"]},
      "object": {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "demo-config", "namespace": "admission-tap-capture-demo", "...": "..."},
        "data": {"greeting": "hello"}
      },
      "oldObject": null,
      "dryRun": false,
      "...": "..."
    }
  },
  "response": {
    "apiVersion": "admission.k8s.io/v1",
    "kind": "AdmissionReview",
    "response": {"uid": "b3c0e7a2-...", "allowed": true}
  }
}
```

The top-level fields from `uid` to `allowed` are a summary lifted out of the request, so you can filter without digging into the payload. `request` is the AdmissionReview exactly as received, and `response` is what capture answered. Every field is described in [Records](records.md#record-format).


## Fetching capture records

With the token set and the port-forward running (see [Query API](query-api.md#connecting)), filter on `webhook=capture`:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/records?webhook=capture&namespace=admission-tap-capture-demo&summary=true'
```

`summary=true` leaves out the HTTP details and the payloads. To see one record in full, ask for it by id:

```bash
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/records/7
```

On stdout, capture lines are the ones with `"webhook": "capture"`:

```bash
kubectl -n admission-tap logs deploy/admission-tap \
  | jq -c 'select(.webhook == "capture") | {id, operation, resource: .resource.resource, subResource, name, username}'
```


## Walkthrough

This uses the files in `examples/capture/`. It assumes admission-tap is deployed with capture on, which is the default.

Follow the server's records in a separate terminal. `select(.webhook)` drops the server's own log lines:

```bash
kubectl -n admission-tap logs deploy/admission-tap -f \
  | jq -c 'select(.webhook) | {id, webhook, operation, resource: .resource.resource, subResource, name, username}'
```

Create the namespace. It has only the capture label, so mutate and validate stay out of the way:

```bash
kubectl apply -f examples/capture/namespace.yaml
```

The creation of the Namespace itself is the first record.

Create a ConfigMap. This produces the record shown above:

```bash
kubectl apply -f examples/capture/configmap.yaml
```

Create a Deployment:

```bash
kubectl apply -f examples/capture/deployment.yaml
```

You get far more than one record. You created the Deployment, but the controllers then create a ReplicaSet and a Pod, the scheduler binds the Pod, and the kubelet and controllers update their status. Each `username` tells you who did what.

Run a command in the Pod. `deploy/web` lets kubectl pick the Deployment's Pod for you:

```bash
kubectl -n admission-tap-capture-demo exec deploy/web -- id
```

This is a CONNECT on `pods`, subresource `exec`. The command is in the recorded request:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/records?webhook=capture&subresource=exec' \
  | jq -c '.items[].request.request.object.command'
```

To clean up, delete the namespace, or run `scripts/undeploy.sh`, which deletes it too:

```bash
kubectl delete namespace admission-tap-capture-demo
```


## Things worth trying

In the walkthrough namespace:

```bash
# An UPDATE: the record has both object and oldObject
kubectl -n admission-tap-capture-demo label configmap demo-config tier=frontend

# A request with dryRun: true. Nothing is stored, but capture still sees it
kubectl -n admission-tap-capture-demo create configmap dry --from-literal=a=b --dry-run=server

# A subresource: UPDATE on deployments/scale
kubectl -n admission-tap-capture-demo scale deployment web --replicas=2

# A Secret: its values are recorded as <redacted>
kubectl -n admission-tap-capture-demo create secret generic demo-secret --from-literal=password=s3cret

# A DELETE: the record has oldObject and no object
kubectl -n admission-tap-capture-demo delete configmap demo-config
```

Then capture a namespace of your own and see what your tools and controllers write there.
