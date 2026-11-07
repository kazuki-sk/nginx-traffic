#!/bin/sh
# Validate fixtures with the real nginx (`nginx -t`) in the official image.
#   testdata/fixtures/*  must pass
#   testdata/invalid/*   must fail
# Usage: scripts/verify-fixtures.sh [image]   (default nginx:1.30.0)
# Runs read-only: the repository is mounted read-only; test certificates are
# created inside the throw-away container. ip_nonlocal_bind (container netns
# only) lets `nginx -t` bind the placeholder internal IP of the sample.
set -eu
IMAGE="${1:-nginx:1.30.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec docker run --rm --network none --sysctl net.ipv4.ip_nonlocal_bind=1 -v "$ROOT:/repo:ro" "$IMAGE" sh -euc '
  mkdir -p /etc/nginx-test /etc/nginx/tls
  # Test-only self-signed certificate (nginx -t loads certificates).
  if command -v openssl >/dev/null 2>&1; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=test \
      -keyout /etc/nginx-test/key.pem -out /etc/nginx-test/cert.pem 2>/dev/null
  else
    echo "openssl not available in image" >&2; exit 2
  fi
  cp /etc/nginx-test/cert.pem /etc/nginx/tls/nginx-map.crt
  cp /etc/nginx-test/key.pem  /etc/nginx/tls/nginx-map.key
  nginx -v
  rc=0
  for d in /repo/testdata/fixtures/*/; do
    n=$(basename "$d")
    if out=$(nginx -t -q -c "$d/nginx.conf" -p "$d" 2>&1); then
      echo "ok    fixtures/$n"
    else
      echo "FAIL  fixtures/$n (expected valid)"; echo "$out" | sed "s/^/      /"; rc=1
    fi
  done
  for d in /repo/testdata/invalid/*/; do
    n=$(basename "$d")
    if out=$(timeout 20 nginx -t -q -c "$d/nginx.conf" -p "$d" 2>&1); then
      echo "FAIL  invalid/$n (expected nginx -t to fail)"; rc=1
    else
      echo "ok    invalid/$n (rejected: $(echo "$out" | grep -m1 -e emerg -e fault | cut -c1-120))"
    fi
  done
  exit $rc
'
