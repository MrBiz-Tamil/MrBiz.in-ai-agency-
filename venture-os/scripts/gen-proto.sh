#!/usr/bin/env bash
# Regenerates gen/ from proto/ with pinned tool versions (installed into ./bin, not your global GOBIN).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
export GOBIN="$PWD/bin"
go install github.com/bufbuild/buf/cmd/buf@v1.50.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1
PATH="$GOBIN:$PATH" buf lint
PATH="$GOBIN:$PATH" buf generate
if [ -d web/node_modules ]; then
  PATH="$GOBIN:$PWD/web/node_modules/.bin:$PATH" buf generate --template buf.gen.web.yaml
fi
