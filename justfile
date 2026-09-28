default:
  just --list

fmt:
  go fmt ./...

test-go:
  go test ./...

build-bridge:
  npm run build:bridge

test-bridge:
  npm run test:bridge

test: test-go test-bridge

# Build locally without changing VERSION or the installed private CLI.
build: build-bridge
  go build -trimpath -ldflags "-X github.com/kazuki-oshino/rilll/internal/version.BuildVersion=$(cat VERSION)" -o bin/rilll ./cmd/rilll

check: test build

version:
  cat VERSION
