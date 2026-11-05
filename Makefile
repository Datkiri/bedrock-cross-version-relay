# Reproducible builds. Everything here is pure Go: CGO_ENABLED=0, no C toolchain.
#
#   make build     # host binary -> dist/xvrelay
#   make test      # go test ./...
#   make bench     # hot-path benchmarks -> dist/bench.txt (ns/op, B/op, allocs/op)
#   make matrix    # 4x4 diagnostic report -> dist/matrix.json
#   make selfcheck # headless self-check client -> dist/selfcheck
#   make itemaudit # offline invisible-item audit -> dist/itemaudit
#   make clientclone # terminal, command-driven headless Bedrock client -> dist/clientclone
#   make release   # all six platform binaries -> dist/
#   make java      # Java translation core -> java/out, java/out-tools
#   make java-check  # compile the Java tree and run its probes
#   make clean

BIN      := xvrelay
PKG      := ./cmd/$(BIN)
DIST     := dist
VERSION  ?= dev
LDFLAGS  := -s -w -X main.buildVersion=$(VERSION)

export CGO_ENABLED := 0
export GOFLAGS     := -buildvcs=false

PLATFORMS := linux/amd64 linux/arm64 linux/arm windows/amd64 darwin/amd64 darwin/arm64

.PHONY: all build test bench vet matrix selfcheck itemaudit clientclone release java java-check java-clean clean fmt

all: build

build:
	@mkdir -p $(DIST)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN) $(PKG)

test:
	go test ./...

# Hot-path benchmarks are opt-in (a plain `make test` never runs them). Three repetitions keep
# the numbers from being a single noisy sample; dist/bench.txt is the reproducible artifact.
bench:
	@mkdir -p $(DIST)
	@go test ./cmd/$(BIN) -run '^$$' -bench . -benchmem -benchtime=200ms -count=3 > $(DIST)/bench.txt 2>&1; status=$$?; cat $(DIST)/bench.txt; exit $$status

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

# Opt-in diagnostic: 16 protocol pairs x 8 checks. Findings are to-do items, not crashes.
matrix:
	@mkdir -p $(DIST)
	XVER_MATRIX_REPORT=$(DIST)/matrix.json go test ./cmd/$(BIN) -run TestProtocolCompatibilityMatrix -v

# Independent headless Bedrock client used for live self-checks against a running relay.
selfcheck:
	@mkdir -p $(DIST)
	go build -trimpath -o $(DIST)/selfcheck ./cmd/selfcheck

# Offline audit: logs exactly which items become AIR for each client protocol.
itemaudit:
	@mkdir -p $(DIST)
	go build -trimpath -o $(DIST)/itemaudit ./cmd/itemaudit

# Terminal-only, command-driven Bedrock client for live relay/BDS tests without a GUI.
clientclone:
	@mkdir -p $(DIST)
	go build -trimpath -o $(DIST)/clientclone ./cmd/clientclone

release:
	@mkdir -p $(DIST)
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		out=$(DIST)/$(BIN)-$$os-$$arch$$ext; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(PKG); \
	done

# Java side. Translation core only: it compiles and its probes pass, but it
# cannot relay a session yet. See java/README.md for the honest scope.
java:
	@cd java && ./build.sh build

java-check:
	@cd java && ./build.sh check

java-clean:
	@cd java && ./build.sh clean

clean:
	rm -rf $(DIST)
