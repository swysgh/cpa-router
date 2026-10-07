GO ?= go
BUILD_FLAGS := -buildvcs=false -buildmode=c-shared

.PHONY: build test vet fmt clean

build:
	CGO_ENABLED=1 $(GO) build $(BUILD_FLAGS) -o bin/cpa-router.so .

test:
	CGO_ENABLED=1 $(GO) test ./... -race

vet:
	CGO_ENABLED=1 $(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
