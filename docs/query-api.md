# Query API

The server keeps recent records in memory and serves them over plain HTTP on port 8080. The Service exposes it on port 80.

There's one record per call the server receives, from any of its three webhooks. [Records](records.md) describes their format.

The API is never served on the webhook port (8443).


## Connecting

### The token

Every `/api/*` request needs a bearer token. It's stored under the key `token` of the Secret `admission-tap-api-token`.

Secret values are base64-encoded, so decode it as you read it:

```bash
export TOKEN=$(kubectl -n admission-tap get secret admission-tap-api-token \
  -o jsonpath='{.data.token}' | base64 -d)
```

The deploy script also keeps a copy in `certs/api-token`.

### From your machine

Forward local port 8080 to the Service's port 80. Leave it running in its own terminal:

```bash
kubectl -n admission-tap port-forward svc/admission-tap 8080:80
```

Then:

```bash
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/stats
```

### From inside the cluster

Start a temporary curl Pod, deleted when it exits (`--rm`), and pass the token in as an environment variable:

```bash
kubectl run curl --rm -it --restart=Never --image=curlimages/curl:8.10.1 --env TOKEN=$TOKEN -- \
  sh -c 'curl -s -H "Authorization: Bearer $TOKEN" http://admission-tap.admission-tap.svc/api/stats'
```

The single quotes make sure `$TOKEN` is expanded inside the Pod, not by your shell.


## Endpoints

`GET /` prints a short help text. No token needed.

`GET /healthz` returns `ok`. No token needed.

`GET /api/stats` returns counters and store usage:

- `recordEnabled` and `redactSecrets`, the current settings of `-record` and `-redact-secrets`.
- `stored` and `storedBytes`, what the store holds now.
- `maxEntries` and `maxBytes`, the store limits.
- `total`, `evicted`, `denied` and `errors`, counted since the server started.
- `byWebhook` and `byOperation`, totals broken down, such as `{"capture": 120, "mutate": 3, "validate": 3}`.

`GET /api/records` returns `{"count": N, "items": [...]}`, newest first. See the filters below.

`GET /api/records/{id}` returns one full record. It returns 404 if the record never existed or was evicted.

`DELETE /api/records` clears the store and returns `{"removed": N}`. The counters in `/api/stats` are kept.


## Filters

Filters for `/api/records` are optional. They combine with AND, and matching ignores case.

Exact match:

- `webhook`: `mutate`, `validate` or `capture`.
- `namespace` and `name`.
- `kind`: the Kind, such as `Pod`.
- `resource`: the plural resource, such as `pods`.
- `subresource`: such as `exec` or `status`.
- `operation`: `CREATE`, `UPDATE`, `DELETE` or `CONNECT`.

Other filters:

- `user`: part of the username.
- `allowed=true` or `allowed=false`. Failed calls have no answer, so they never match.
- `errors=true`: only failed calls. `errors=false`: only calls without an error. Without the parameter, there is no filter.
- `since`: a record id, to get newer records, or an RFC3339 time.

Output:

- `summary=true` drops the HTTP metadata and the payloads (see [Summaries](records.md#summaries)). It keeps `uid`, `username`, `groups`, `dryRun`, `allowed` and, for failed calls, `error`.
- `limit` defaults to 50. Full records are capped at 100. With `summary=true`, the cap is the store size. Larger values are lowered to the cap, not rejected.

Invalid values return 400 with `{"error": "..."}`. A missing or wrong token returns 401.


## Examples

These assume the port-forward is running and `TOKEN` is set. Store the header in a variable to keep lines short:

```bash
A="Authorization: Bearer $TOKEN"
```

What's happening right now:

```bash
curl -s -H "$A" 'localhost:8080/api/records?summary=true&limit=20'
```

Who deleted a Secret:

```bash
curl -s -H "$A" 'localhost:8080/api/records?kind=Secret&operation=DELETE&summary=true'
```

Requests the validate webhook denied:

```bash
curl -s -H "$A" 'localhost:8080/api/records?webhook=validate&allowed=false&summary=true'
```

Everything capture saw in one namespace:

```bash
curl -s -H "$A" 'localhost:8080/api/records?webhook=capture&namespace=admission-tap-capture-demo&summary=true'
```

One record in full, by id:

```bash
curl -s -H "$A" localhost:8080/api/records/42
```

Calls that failed:

```bash
curl -s -H "$A" 'localhost:8080/api/records?errors=true'
```

Poll for records newer than id 120:

```bash
curl -s -H "$A" 'localhost:8080/api/records?since=120&summary=true'
```

Commands run with `kubectl exec`. jq digs the command out of each recorded request:

```bash
curl -s -H "$A" 'localhost:8080/api/records?subresource=exec&webhook=capture' \
  | jq -c '.items[].request.request.object.command'
```

The latest patch from the mutate webhook. Patches are base64-encoded, so jq extracts it and `base64 -d` decodes it:

```bash
curl -s -H "$A" 'localhost:8080/api/records?webhook=mutate&limit=1' \
  | jq -r '.items[0].response.response.patch' | base64 -d
```

Clear the store:

```bash
curl -s -X DELETE -H "$A" localhost:8080/api/records
```
