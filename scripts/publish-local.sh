#!/usr/bin/env bash
set -euo pipefail

REGISTRY="http://localhost:4873"
PKG_DIR="sdk/node"

# Build the messaging Docker image so the sidecar includes the latest Go changes
echo "Building messaging-local Docker image..."
docker build -t messaging-local . --quiet
echo "✅ messaging-local image built"

# Create a unique Verdaccio user to get a fresh token
USER="ci-$(date +%s)"
PASS="local"

TOKEN=$(curl -sf -XPUT \
  -H "Content-type: application/json" \
  -d "{\"name\":\"$USER\",\"password\":\"$PASS\"}" \
  "$REGISTRY/-/user/org.couchdb.user:$USER" | jq -r '.token')

if [ -z "$TOKEN" ]; then
  echo "Error: could not get auth token from Verdaccio at $REGISTRY" >&2
  echo "Is Verdaccio running? (docker compose up -d verdaccio)" >&2
  exit 1
fi

AUTH="--registry $REGISTRY --//localhost:4873/:_authToken=$TOKEN"

PKG_NAME=$(cd "$PKG_DIR" && node -p "require('./package.json').name")
PKG_VERSION=$(cd "$PKG_DIR" && node -p "require('./package.json').version")

# Remove any existing version (local or proxy-cached) so we can republish
npm unpublish "$PKG_NAME@$PKG_VERSION" $AUTH --force 2>/dev/null || true

echo "Publishing $PKG_DIR ($PKG_NAME@$PKG_VERSION) to $REGISTRY..."

# Strip publishConfig.registry so --registry flag takes effect
cp "$PKG_DIR/package.json" "$PKG_DIR/package.json.bak"
cd "$PKG_DIR" && node -e "
  const fs = require('fs');
  const pkg = JSON.parse(fs.readFileSync('package.json','utf8'));
  delete pkg.publishConfig;
  fs.writeFileSync('package.json', JSON.stringify(pkg, null, 2) + '\n');
"
npm publish $AUTH
cd - > /dev/null
mv "$PKG_DIR/package.json.bak" "$PKG_DIR/package.json"

echo "✅ $PKG_NAME@$PKG_VERSION published to $REGISTRY"
