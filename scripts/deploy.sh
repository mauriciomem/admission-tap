#!/usr/bin/env bash
# Deploys admission-tap to the cluster kubectl currently points at.
#
# Usage:  IMG=registry.example.com/admission-tap:v1 \
#         [MUTATE_VALIDATE=true|false] [CAPTURE=true|false] scripts/deploy.sh
#
#   IMG              (required) container image to run
#   MUTATE_VALIDATE  (default true) register the example mutate/validate
#                    webhooks. false: don't register them (previously
#                    registered ones are deleted).
#   CAPTURE          (default true) register the capture webhook.
#                    false: don't register it (a previously registered one is
#                    deleted); mutate/validate keep working and the server
#                    keeps recording their calls.
#
# The server runs the same way whatever these are set to, and records every
# call it receives. They only decide which webhooks send it calls.
#
# Safe to re-run: every step creates or updates.
set -euo pipefail

# Fixed: the certificate SANs and the manifests all hardcode this namespace.
readonly NAMESPACE=admission-tap

# Always work from the repo root, wherever the script is called from.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMG=${IMG:-}
MUTATE_VALIDATE=${MUTATE_VALIDATE:-true}
CAPTURE=${CAPTURE:-true}

if [[ -z "$IMG" ]]; then
  echo "error: IMG is required" >&2
  echo "usage: IMG=<image> [MUTATE_VALIDATE=true|false] [CAPTURE=true|false] $0" >&2
  exit 1
fi
if [[ "$MUTATE_VALIDATE" != "true" && "$MUTATE_VALIDATE" != "false" ]]; then
  echo "error: MUTATE_VALIDATE must be true or false (got '$MUTATE_VALIDATE')" >&2
  exit 1
fi
if [[ "$CAPTURE" != "true" && "$CAPTURE" != "false" ]]; then
  echo "error: CAPTURE must be true or false (got '$CAPTURE')" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
echo "==> 1/9 Generate the CA and server certificate"
# The API server only talks to webhooks over TLS, so we need a CA it can trust
# and a server certificate for the Service name. gen-certs.sh does nothing if
# certs/ already holds them, so re-running keeps the same CA.
scripts/gen-certs.sh

# ---------------------------------------------------------------------------
echo "==> 2/9 Generate the API token"
# The query API is protected by a random bearer token. Keep it in certs/
# (gitignored) so re-running the script doesn't rotate it.
if [[ ! -f certs/api-token ]]; then
  (umask 077 && openssl rand -hex 32 > certs/api-token)
  echo "wrote certs/api-token"
else
  echo "certs/api-token already exists"
fi

# ---------------------------------------------------------------------------
echo "==> 3/9 Create the namespace"
# The Secrets and the Deployment live here, so it must exist first.
kubectl apply -f deploy/00-namespace.yaml

# ---------------------------------------------------------------------------
echo "==> 4/9 Create the TLS Secret"
# 'create --dry-run=client -o yaml | apply' makes this idempotent: a plain
# 'create' fails if the Secret exists, 'apply' creates or updates it.
kubectl -n "$NAMESPACE" create secret tls admission-tap-tls \
  --cert=certs/tls.crt \
  --key=certs/tls.key \
  --dry-run=client -o yaml \
  | kubectl apply -f -

# ---------------------------------------------------------------------------
echo "==> 5/9 Create the API token Secret"
# Same create-or-update idiom. The server reads the token from key 'token'.
kubectl -n "$NAMESPACE" create secret generic admission-tap-api-token \
  --from-file=token=certs/api-token \
  --dry-run=client -o yaml \
  | kubectl apply -f -

# ---------------------------------------------------------------------------
echo "==> 6/9 Deploy the server"
# The Pods read the TLS certificate and the token only at startup, so a changed
# Secret alone doesn't restart them. Hash the three files into a Pod template
# annotation: a changed hash triggers a rolling restart, an unchanged one leaves
# the Deployment untouched. (openssl is already required; unlike sha256sum vs
# shasum, it behaves the same on Linux and macOS.)
SECRETS_CHECKSUM=$(cat certs/tls.crt certs/tls.key certs/api-token \
  | openssl dgst -sha256 -r | cut -d' ' -f1)

# The manifest has placeholders for the image and the checksum.
sed \
  -e "s|__IMAGE__|${IMG}|" \
  -e "s|__SECRETS_CHECKSUM__|${SECRETS_CHECKSUM}|" \
  deploy/10-server.yaml \
  | kubectl apply -f -

# ---------------------------------------------------------------------------
echo "==> 7/9 Wait for the server to be Ready"
# Webhooks must not be registered before the server is Ready: with
# failurePolicy Fail, the API server would reject requests to a server that
# isn't answering yet.
kubectl -n "$NAMESPACE" rollout status deploy/admission-tap --timeout=120s

# 'rollout status' returns as soon as the new Pod is Ready, but on an update the
# old Pod is still Terminating: it keeps serving for -shutdown-delay (5s). Until
# it is gone, 'kubectl logs deploy/admission-tap' and 'kubectl port-forward
# svc/admission-tap' can still pick the old Pod. Wait (up to 30s) until exactly
# one Pod is left; as the rollout is complete, that one is the new, Ready Pod.
# On timeout, warn and carry on: the deployment itself is fine.
for _ in $(seq 30); do
  pods=$(kubectl -n "$NAMESPACE" get pods -l app=admission-tap --no-headers 2>/dev/null | wc -l | tr -d ' ' || true)
  [[ "$pods" == "1" ]] && break
  sleep 1
done
if [[ "$pods" != "1" ]]; then
  echo "warning: ${pods:-?} admission-tap Pods still exist after 30s; kubectl logs/port-forward may reach an old Pod" >&2
fi

# The API server verifies the server certificate against this CA (base64, one line).
CA_BUNDLE=$(base64 < certs/ca.crt | tr -d '\n')

# ---------------------------------------------------------------------------
echo "==> 8/9 Mutate/validate webhooks (MUTATE_VALIDATE=${MUTATE_VALIDATE})"
if [[ "$MUTATE_VALIDATE" == "true" ]]; then
  # Mutates and validates Pods in namespaces labelled admission-tap=enabled.
  sed "s|__CA_BUNDLE__|${CA_BUNDLE}|" deploy/20-webhooks.yaml \
    | kubectl apply -f -
else
  # Remove webhooks left over from an earlier run with MUTATE_VALIDATE=true.
  # They have failurePolicy Fail, so leaving them would keep Pod creation in
  # labelled namespaces dependent on this server.
  kubectl delete mutatingwebhookconfiguration admission-tap --ignore-not-found
  kubectl delete validatingwebhookconfiguration admission-tap --ignore-not-found
fi

# ---------------------------------------------------------------------------
echo "==> 9/9 Capture webhook (CAPTURE=${CAPTURE})"
if [[ "$CAPTURE" == "true" ]]; then
  # Records every write in namespaces labelled admission-tap/capture=enabled.
  sed "s|__CA_BUNDLE__|${CA_BUNDLE}|" deploy/30-capture.yaml \
    | kubectl apply -f -
else
  # Remove a capture webhook left over from an earlier run with CAPTURE=true.
  kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
fi

echo
echo "Done. Next steps:"

# Only suggest opt-in labels for webhooks that are actually registered.
if [[ "$CAPTURE" == "true" || "$MUTATE_VALIDATE" == "true" ]]; then
  echo
  echo "  # Opt namespaces in (nothing is intercepted until a namespace is labelled):"
  if [[ "$CAPTURE" == "true" ]]; then
    echo "  kubectl label namespace <ns> admission-tap/capture=enabled   # capture webhook"
  fi
  if [[ "$MUTATE_VALIDATE" == "true" ]]; then
    echo "  kubectl label namespace <ns> admission-tap=enabled           # mutate/validate webhooks"
  fi
fi

cat <<EOF

  # Read the API token:
  kubectl -n ${NAMESPACE} get secret admission-tap-api-token -o jsonpath='{.data.token}' | base64 -d; echo

  # Reach the query API on http://localhost:8080 (records are at /api/records):
  kubectl -n ${NAMESPACE} port-forward svc/admission-tap 8080:80
EOF

if [[ "$MUTATE_VALIDATE" == "false" && "$CAPTURE" == "false" ]]; then
  cat <<EOF

Note: no webhooks are registered (MUTATE_VALIDATE=false and CAPTURE=false),
so the server receives no admission calls and records nothing.
EOF
fi
