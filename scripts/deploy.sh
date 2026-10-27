#!/usr/bin/env bash
# Deploys admission-tap to the cluster kubectl currently points at.
#
# Usage:  IMG=registry.example.com/admission-tap:v1 [CAPTURE=true|false] scripts/deploy.sh
#
#   IMG      (required) container image to run
#   CAPTURE  (default true) register the capture webhook, and run the server
#            with capture enabled
#
# Safe to re-run: every step creates or updates.
set -euo pipefail

# Fixed: the certificate SANs and the manifests all hardcode this namespace.
readonly NAMESPACE=admission-tap

# Always work from the repo root, wherever the script is called from.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMG=${IMG:-}
CAPTURE=${CAPTURE:-true}

if [[ -z "$IMG" ]]; then
  echo "error: IMG is required" >&2
  echo "usage: IMG=<image> [CAPTURE=true|false] $0" >&2
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

# The manifest has placeholders for the image, the capture flag and the checksum.
sed \
  -e "s|__IMAGE__|${IMG}|" \
  -e "s|__CAPTURE__|${CAPTURE}|" \
  -e "s|__SECRETS_CHECKSUM__|${SECRETS_CHECKSUM}|" \
  deploy/10-server.yaml \
  | kubectl apply -f -

# ---------------------------------------------------------------------------
echo "==> 7/9 Wait for the server to be Ready"
# Webhooks must not be registered before the server is Ready: with
# failurePolicy Fail, the API server would reject requests to a server that
# isn't answering yet.
kubectl -n "$NAMESPACE" rollout status deploy/admission-tap --timeout=120s

# The API server verifies the server certificate against this CA (base64, one line).
CA_BUNDLE=$(base64 < certs/ca.crt | tr -d '\n')

# ---------------------------------------------------------------------------
echo "==> 8/9 Register the example webhooks"
# Mutate and validate Pods in namespaces labelled admission-tap=enabled.
sed "s|__CA_BUNDLE__|${CA_BUNDLE}|" deploy/20-webhooks.yaml \
  | kubectl apply -f -

# ---------------------------------------------------------------------------
echo "==> 9/9 Capture webhook (CAPTURE=${CAPTURE})"
if [[ "$CAPTURE" == "true" ]]; then
  # Captures admission calls in namespaces labelled admission-tap/capture=enabled.
  sed "s|__CA_BUNDLE__|${CA_BUNDLE}|" deploy/30-capture.yaml \
    | kubectl apply -f -
else
  # Remove a capture webhook left over from an earlier run with CAPTURE=true.
  kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
fi

cat <<EOF

Done. Next steps:

  # Capture a namespace (nothing is captured until a namespace is labelled):
  kubectl label namespace <ns> admission-tap/capture=enabled

  # Read the API token:
  kubectl -n ${NAMESPACE} get secret admission-tap-api-token -o jsonpath='{.data.token}' | base64 -d; echo

  # Reach the query API on http://localhost:8080:
  kubectl -n ${NAMESPACE} port-forward svc/admission-tap 8080:80
EOF
