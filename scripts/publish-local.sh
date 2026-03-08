#!/usr/bin/env bash
set -euo pipefail

REGISTRY="http://localhost:4873"
PKG_DIR="sdk/node"

# Create a unique Verdaccio user to get a fresh token
USER="ci-$(date +%s)"
PASS="local"

TOKEN=$(curl -sf -XPUT \
  -H "Content-type: application/json" \
  -d "{\"name\":\"$USER\",\"password\":\"$PASS\"}" \
  "$REGISTRY/-/user/org.couchdb.user:$USER" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")

if [ -z "$TOKEN" ]; then
  echo "Error: could not get auth token from Verdaccio at $REGISTRY" >&2
  echo "Is Verdaccio running? (docker compose up -d verdaccio)" >&2
  exit 1
fi

AUTH="--registry $REGISTRY --//localhost:4873/:_authToken=$TOKEN"

PKG_NAME=$(cd "$PKG_DIR" && node -p "require('./package.json').name")
PKG_VERSION=$(cd "$PKG_DIR" && node -p "require('./package.json').version")

# Delete existing version from Verdaccio storage
curl -sf -XDELETE \
  -H "Authorization: Bearer $TOKEN" \
  "$REGISTRY/$PKG_NAME/-rev/whatever" 2>/dev/null || true

echo "Publishing $PKG_DIR ($PKG_NAME@$PKG_VERSION) to $REGISTRY..."
(cd "$PKG_DIR" && npm publish $AUTH)

echo "✅ $PKG_NAME@$PKG_VERSION published to $REGISTRY"
