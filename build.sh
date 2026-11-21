#!/bin/sh
# Run on your dev machine (needs Go 1.22+ and internet access for modules).
set -e
cd "$(dirname "$0")"
go mod tidy
rm -rf dist
mkdir -p dist
GOOS=darwin  GOARCH=arm64 go build -o dist/jellyfin-proxy-mac-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/jellyfin-proxy-mac-intel .
GOOS=windows GOARCH=amd64 go build -o dist/jellyfin-proxy.exe .
GOOS=linux   GOARCH=amd64 go build -o dist/jellyfin-proxy-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/jellyfin-proxy-linux-arm64 .