BIN := bin
GOFLAGS :=

.PHONY: all build cli agent agent-linux clean fmt vet test tidy

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

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN)
