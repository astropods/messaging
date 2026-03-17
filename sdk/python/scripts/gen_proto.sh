#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROTO_SRC="$SCRIPT_DIR/../../../proto"
OUT_DIR="$SCRIPT_DIR/../src/astropods_messaging"

if ! python -c "import grpc_tools" 2>/dev/null; then
  echo "grpcio-tools not installed. Run: pip install grpcio-tools"
  exit 1
fi

GRPC_TOOLS_DIR="$(python -c 'import grpc_tools, os; print(os.path.dirname(grpc_tools.__file__))')"

python -m grpc_tools.protoc \
  -I "$PROTO_SRC" \
  -I "$GRPC_TOOLS_DIR/_proto" \
  --python_out="$OUT_DIR" \
  --grpc_python_out="$OUT_DIR" \
  "$PROTO_SRC"/astro/messaging/v1/*.proto

# Fix generated imports to use relative imports within the package
for f in "$OUT_DIR"/astro/messaging/v1/*_pb2*.py; do
  sed -i.bak 's/^from astro\.messaging\.v1 import \([a-z_]*_pb2\)/from . import \1/' "$f"
  sed -i.bak 's/^import \([a-z_]*_pb2\)/from . import \1/' "$f"
  rm -f "$f.bak"
done

# Create __init__.py files for the namespace packages
touch "$OUT_DIR/astro/__init__.py"
touch "$OUT_DIR/astro/messaging/__init__.py"
touch "$OUT_DIR/astro/messaging/v1/__init__.py"

echo "Proto stubs generated in $OUT_DIR/astro/messaging/v1/"
