#!/usr/bin/env bash
# Removes admission-tap from the cluster kubectl currently points at.
#
# Usage:  scripts/undeploy.sh
#
# Safe to re-run. Local files in certs/ are kept.
set -euo pipefail

# Always work from the repo root, wherever the script is called from.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# ---------------------------------------------------------------------------
echo "==> 1/3 Delete the capture webhook"
# Webhook configurations go FIRST so the API server stops calling a server
# that is about to disappear.
kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found

# ---------------------------------------------------------------------------
echo "==> 2/3 Delete the example webhooks"
# Same reason as above. These have failurePolicy Fail, so leaving them behind
# would block Pod creation in labelled namespaces.
kubectl delete validatingwebhookconfiguration admission-tap --ignore-not-found
kubectl delete mutatingwebhookconfiguration admission-tap --ignore-not-found

# ---------------------------------------------------------------------------
echo "==> 3/3 Delete the namespaces"
# Removes the demo namespace and the server's namespace with everything in it
# (Deployment, Service, Secrets).
kubectl delete namespace admission-tap-demo admission-tap --ignore-not-found

echo
echo "Done. Local certificates and the API token were kept."
echo "To remove them too: rm -rf certs"
