# Reproducible builds. Everything here is pure Go: CGO_ENABLED=0, no C toolchain.
#
#   make build     # host binary -> dist/xvrelay
#   make test      # go test ./...
#   make matrix    # 4x4 diagnostic report -> dist/matrix.json
#   make release   # all six platform binaries -> dist/
#   make clean

BIN      := xvrelay
PKG      := ./cmd/$(BIN)
DIST     := dist
VERSION  ?= dev
LDFLAGS  := -s -w -X main.buildVersion=$(VERSION)

export CGO_ENABLED := 0
export GOFLAGS     := -buildvcs=false

PLATFORMS := linux/amd64 linux/arm64 linux/arm windows/amd64 darwin/amd64 darwin/arm64

.PHONY: all build test vet matrix release clean fmt

all: build

build:
	@mkdir -p $(DIST)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

# Opt-in diagnostic: 16 protocol pairs x 7 checks. Findings are to-do items, not crashes.
matrix:
	@mkdir -p $(DIST)
	XVER_MATRIX_REPORT=$(DIST)/matrix.json go test ./cmd/$(BIN) -run TestProtocolCompatibilityMatrix -v

release:
	@mkdir -p $(DIST)
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		out=$(DIST)/$(BIN)-$$os-$$arch$$ext; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(PKG); \
	done

clean:
	rm -rf $(DIST)
