#!/bin/sh
# Makes the self-signed certificate of the ftps test server in dev/tls/ (not
# in git; DSFetch pins it on first use). An existing one is kept.
set -e
dir="$(dirname "$0")/tls"
if [ -f "$dir/cert.pem" ] && [ -f "$dir/key.pem" ]; then
    exit 0
fi
mkdir -p "$dir"
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=dsfetch-dev-ftps" \
    -keyout "$dir/key.pem" -out "$dir/cert.pem" 2>/dev/null
echo "created $dir/cert.pem"
