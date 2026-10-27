#!/usr/bin/env bash
# Creates a self-signed CA and a server certificate for the webhook Service.
# The API server calls https://<SVC>.<NS>.svc, so that name must be in the SAN.
set -euo pipefail

SVC=${SVC:-admission-tap}
NS=${NS:-admission-tap}
DIR=${DIR:-certs}
EXTRA_SANS=${EXTRA_SANS:-}   # e.g. "DNS:localhost,IP:127.0.0.1" for local testing

mkdir -p "$DIR"
cd "$DIR"

if [[ -f tls.crt && -f ca.crt ]]; then
  echo "certs already exist in $DIR (delete the directory to regenerate)"
  exit 0
fi

# Key generation prints progress dots ("..+++") on stderr, and not every
# openssl (LibreSSL) has -quiet. So hide stderr, but show it if the command fails.
quiet() {
  local log
  log=$(mktemp)
  if ! "$@" 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    return 1
  fi
  rm -f "$log"
}

quiet openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout ca.key -out ca.crt -subj "/CN=admission-tap-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

SANS="DNS:${SVC},DNS:${SVC}.${NS},DNS:${SVC}.${NS}.svc,DNS:${SVC}.${NS}.svc.cluster.local"
[[ -n "$EXTRA_SANS" ]] && SANS="${SANS},${EXTRA_SANS}"

cat > server.ext <<EOF
subjectAltName = ${SANS}
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
EOF

quiet openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr -subj "/CN=${SVC}.${NS}.svc"
quiet openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 365 -out tls.crt -extfile server.ext

rm -f tls.csr server.ext ca.srl
echo "wrote $DIR/ca.crt $DIR/tls.crt $DIR/tls.key"
