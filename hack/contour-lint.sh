#! /usr/bin/env bash

readonly PROGNAME="lint"

GOPATH=${GOPATH:-$(go env GOPATH)}
GOBIN=${GOBIN:-$(go env GOBIN)}
GOBIN=${GOBIN:-${GOPATH}/bin}

go install github.com/projectcontour/lint

"${GOBIN}"/lint ./...

