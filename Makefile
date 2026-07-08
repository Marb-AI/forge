BIN := bin
DIST := dist
AGENTBIN := internal/agentbin
GOFLAGS :=

.PHONY: all build cli agent agent-linux release clean fmt vet test tidy

all: build

build: cli agent

cli:
	go build $(GOFLAGS) -o $(BIN)/forge ./cmd/forge

agent:
	go build $(GOFLAGS) -o $(BIN)/forge-agent ./cmd/forge-agent

# The agent runs on the (Linux) server; cross-compile it from the laptop.
agent-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BIN)/forge-agent-linux-amd64 ./cmd/forge-agent
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(BIN)/forge-agent-linux-arm64 ./cmd/forge-agent

# Release: embed both linux agents into the CLI (so a single `forge` carries the
# agent for every server arch), then build the CLI for each supported OS/arch.
# Windows is pending a shim for the daemon-detach/signal syscalls.
release: agent-linux
	cp $(BIN)/forge-agent-linux-amd64 $(AGENTBIN)/forge-agent-linux-amd64
	cp $(BIN)/forge-agent-linux-arm64 $(AGENTBIN)/forge-agent-linux-arm64
	@for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  forge $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -tags embedagent -o $(DIST)/forge-$$os-$$arch ./cmd/forge || exit 1; \
	done
	@rm -f $(AGENTBIN)/forge-agent-linux-amd64 $(AGENTBIN)/forge-agent-linux-arm64
	@echo "release binaries in $(DIST)/"

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN) $(DIST) $(AGENTBIN)/forge-agent-linux-amd64 $(AGENTBIN)/forge-agent-linux-arm64
