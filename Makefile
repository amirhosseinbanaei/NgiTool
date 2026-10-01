# NgiTool build. `make check` is the gate every change passes.

export PATH := $(PATH):/usr/local/go/bin

GO      ?= go
MODULE  := github.com/amirhosseinbanaei/NgiTool
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# The size budget for dist/ngitool (linux/amd64), in bytes: 13 MB (raised
# from 12 MB in prompt 3; the reason is in AGENTS.md).
MAX_SIZE := 13000000

.PHONY: build size fmt vet test check release-local clean

## build: dist/ngitool for linux/amd64, static and stripped
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ngitool ./cmd/ngitool

## size: fail when dist/ngitool is over the budget
size: build
	@size=$$(stat -c %s dist/ngitool); \
	echo "dist/ngitool: $$size bytes (budget $(MAX_SIZE))"; \
	if [ $$size -gt $(MAX_SIZE) ]; then \
		echo "over budget; biggest symbols:"; $(GO) tool nm -size -sort size dist/ngitool | head -20; exit 1; \
	fi

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

## check: gofmt, vet, tests, build, size
check:
	test -z "$$(gofmt -l .)" && $(GO) vet ./... && $(GO) test ./... && $(MAKE) build && $(MAKE) size

## release-local: the GoReleaser archive names plus checksums.txt, with
## plain go build, tar and sha256sum, into dist/release/<VERSION>/
release-local:
	@if [ "$(VERSION)" = dev ]; then echo "usage: make release-local VERSION=vX.Y.Z"; exit 1; fi
	@set -e; v=$(VERSION); n=$${v#v}; out=dist/release/$$v; \
	rm -rf $$out; mkdir -p $$out; \
	for t in amd64:amd64: arm64:arm64: arm:armv7:7; do \
		goarch=$${t%%:*}; rest=$${t#*:}; arch=$${rest%%:*}; goarm=$${rest#*:}; \
		stage=$$out/.stage-$$arch; mkdir -p $$stage; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch GOARM=$$goarm \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$stage/ngitool ./cmd/ngitool; \
		tar -C $$stage -czf $$out/ngitool_$${n}_linux_$$arch.tar.gz ngitool; \
		rm -rf $$stage; \
	done; \
	cd $$out && sha256sum *.tar.gz > checksums.txt && cat checksums.txt

clean:
	rm -rf dist
