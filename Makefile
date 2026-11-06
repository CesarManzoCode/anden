# Unique check entry point: format, types, tests and package boundaries.
# It needs no privileges and no external tool beyond the Go toolchain and Python.
GO ?= go
PROTOC_GEN_GO_VERSION := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2
RUNTIME_TOOLS_COMMIT := bd3b0bd00f44e15f0f1ff4130c61dbea1b1c56cc
ANDEN_CACHE ?= $(CURDIR)/.cache
GEN_OUT ?= gen

.PHONY: check build gen tools accept

check:
	@if [ ! -f go.mod ]; then echo "check: no product yet, nothing to verify"; exit 0; fi; \
	unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi; \
	$(GO) vet ./... && $(GO) test ./... || exit 1; \
	if [ -d verify ] && $(GO) list -f '{{.ImportPath}}: {{join .Imports " "}}' ./verify/... | grep -q '/anden/internal/'; then \
		echo "boundary violation: verify/ must not import internal/"; exit 1; \
	fi; \
	if [ -d acceptance ]; then python3 -m compileall -q acceptance || exit 1; fi

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -o bin/ ./cmd/... ./verify/cmd/...

gen:
	mkdir -p $(ANDEN_CACHE)/tools $(GEN_OUT)
	GOBIN=$(ANDEN_CACHE)/tools $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(ANDEN_CACHE)/tools $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	PATH=$(ANDEN_CACHE)/tools:$$PATH protoc -I api --go_out=$(GEN_OUT) --go_opt=paths=source_relative \
		--go-grpc_out=$(GEN_OUT) --go-grpc_opt=paths=source_relative api/anden/v1/anden.proto

tools:
	python3 acceptance/fetch_tools.py --cache $(ANDEN_CACHE) --runtime-tools-commit $(RUNTIME_TOOLS_COMMIT)

accept: build tools
	python3 acceptance/run_all.py
