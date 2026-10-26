# Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| `x509: certificate signed by unknown authority` | `caBundle` doesn't match the CA that signed the server certificate | `make undeploy`, `rm -rf certs`, `make deploy IMG=...` |
| `x509: certificate is valid for ..., not admission-tap.admission-tap.svc` | Service name or namespace changed without regenerating certs | Check with `openssl x509 -in certs/tls.crt -noout -ext subjectAltName`, then regenerate |
| `failed calling webhook ... connection refused` or `no endpoints available` | Server Pod not ready, wrong image, or Service mismatch | `kubectl -n admission-tap get pods,endpoints` and `kubectl -n admission-tap describe pod` |
| `context deadline exceeded` | The API server can't reach the Pod network (CNI, firewall, NetworkPolicy) | Check connectivity from the control plane and any NetworkPolicies in `admission-tap` |
| Pod in `ImagePullBackOff` | Image not pushed, private, wrong architecture, or not loaded into kind | Check `IMG`, registry visibility, and `--platform` |
| No `capture` records | Deployed with `CAPTURE=false`, or capture webhook calls are failing silently (`failurePolicy: Ignore`) | `kubectl get validatingwebhookconfiguration admission-tap-capture` and `/api/stats` |
| Your record is gone | Evicted by cluster noise, or the Pod restarted | Filter more narrowly and query sooner, or raise the store limits and the memory limit |
| `401 missing or invalid bearer token` | No `Authorization` header, or a stale token | `export TOKEN=$(make -s token)` and send `-H "Authorization: Bearer $TOKEN"` |
| Pod crashes with `query API token` in the logs | Token Secret missing or empty | `make deploy` recreates it from `certs/api-token`; delete that file to get a new token |
| `make api` fails | Server not running, or port 8080 already in use locally | `kubectl -n admission-tap get pods` |

The API server logs webhook failures too. On kubeadm: `kubectl -n kube-system logs kube-apiserver-<node> | grep admission-tap`.

## Emergency removal

If a webhook is blocking things, delete its configuration. The API server stops calling it right away. This always works, because webhook configurations are never sent to webhooks.

```bash
kubectl delete mutatingwebhookconfiguration admission-tap
kubectl delete validatingwebhookconfiguration admission-tap admission-tap-capture
```

Then fix the server and run `make deploy` again, or clean up with `make undeploy`.
