#!/bin/sh
set -e
cd "$(dirname "$0")"
go mod tidy
rm -rf dist
mkdir -p dist
GOOS=darwin  GOARCH=arm64 go build -o dist/tailgate-mac-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/tailgate-mac-intel .
GOOS=windows GOARCH=amd64 go build -o dist/tailgate.exe .
GOOS=linux   GOARCH=amd64 go build -o dist/tailgate-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/tailgate-linux-arm64 .
