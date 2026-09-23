.PHONY: vendor
vendor:
	go mod tidy && go mod vendor

.PHONY: env
env:
	cp .env.example .env

################
# BUILD BINARY
################

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
CGO_ENABLED ?= 0

GO_BUILD_FLAGS=-trimpath -mod=vendor

.PHONY: build/worker
build/worker:
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_BUILD_FLAGS) -o build/worker ./cmd

################
# UNIT TEST
################

.PHONY: test
test:
	go test -mod=vendor -race -count=1 ./...
