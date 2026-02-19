#!/bin/bash

# Script to generate Go code from protobuf definitions

set -e

# Get the directory of this script
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Directories
PROTO_DIR="$PROJECT_ROOT/proto"
OUT_DIR="$PROJECT_ROOT/pkg/gen"

echo "Generating Go code from protobuf definitions..."
echo "Proto dir: $PROTO_DIR"
echo "Output dir: $OUT_DIR"

# Create output directory
mkdir -p "$OUT_DIR"

# Generate Go code
protoc \
  --proto_path="$PROTO_DIR" \
  --go_out="$OUT_DIR" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$OUT_DIR" \
  --go-grpc_opt=paths=source_relative \
  "$PROTO_DIR"/astro/messaging/v1/*.proto

echo "✅ Proto generation complete!"
echo "Generated files in: $OUT_DIR/astro/messaging/v1"
