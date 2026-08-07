BIN := bin
DIST := dist
AGENTBIN := internal/agentbin
GOFLAGS :=

# The release this is built from, stamped into both binaries so a server can say
# which client prepared it. `git describe` is the honest default: a tagged tree
# gives the tag, anything else gives the tag it grew from and a "-dirty" when the
# working tree has changes. Override for a build that is not from this checkout:
#   make release VERSION=v1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/Marb-AI/forge/internal/version.Version=$(VERSION)"

# The macOS SDK builds the webview objects for its own version, and the Go linker
# targets 11.0, which is one warning per object file — dozens of them, burying
# anything worth reading. Saying 11.0 on both sides silences it and keeps the app
# running on the same macOS versions the CLI does. Nothing to set anywhere else.
ifeq ($(shell uname -s),Darwin)
APPENV := CGO_CFLAGS=-mmacosx-version-min=11.0 CGO_LDFLAGS=-mmacosx-version-min=11.0
endif

.PHONY: all build cli agent agent-linux app app-bundle app-zip release clean fmt vet test tidy \
	image image-android shell docker-test

all: build

build: cli agent

cli:
	go build $(GOFLAGS) $(LDFLAGS) -o $(BIN)/forge ./cmd/forge

agent:
	go build $(GOFLAGS) $(LDFLAGS) -o $(BIN)/forge-agent ./cmd/forge-agent

# The desktop shell: the same core in a window instead of a browser tab. Not part
# of `build`, because it is cgo over the system's webview and a plain `make` has
# no business needing Xcode or GTK installed.
#
app:
	$(APPENV) go build $(GOFLAGS) $(LDFLAGS) -o $(BIN)/forge-app ./cmd/forge-app

# Forge.app: the same shell, wrapped so the Finder will open it. Universal, so a
# download runs on whichever Mac it lands on, and ad-hoc signed — see build/bundle.sh
# for why that is the floor rather than a substitute for Developer ID.
app-bundle:
	./build/bundle.sh $(VERSION) $(BIN)

# ditto rather than zip: it is the one that preserves the bundle's symlinks and
# its signature, and a .app that arrives with a broken signature is a .app macOS
# refuses to open.
app-zip: app-bundle
	@mkdir -p $(DIST)
	ditto -c -k --keepParent $(BIN)/Forge.app $(DIST)/Forge-macos-universal.zip
	@echo "  $(DIST)/Forge-macos-universal.zip"

# The agent runs on the (Linux) server; cross-compile it from the laptop.
agent-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) $(LDFLAGS) -o $(BIN)/forge-agent-linux-amd64 ./cmd/forge-agent
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) $(LDFLAGS) -o $(BIN)/forge-agent-linux-arm64 ./cmd/forge-agent

# Release: embed both linux agents into the CLI (so a single `forge` carries the
# agent for every server arch), then build the CLI for each supported OS/arch.
# (Windows client compiles and runs; its forwarding-stop is a hard kill — see
# internal/proc — and is not yet runtime-tested.)
release: agent-linux
	@echo "  version $(VERSION)"
	cp $(BIN)/forge-agent-linux-amd64 $(AGENTBIN)/forge-agent-linux-amd64
	cp $(BIN)/forge-agent-linux-arm64 $(AGENTBIN)/forge-agent-linux-arm64
	@for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		echo "  forge $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -tags embedagent $(LDFLAGS) -o $(DIST)/forge-$$os-$$arch$$ext ./cmd/forge || exit 1; \
	done
	@rm -f $(AGENTBIN)/forge-agent-linux-amd64 $(AGENTBIN)/forge-agent-linux-arm64
	@echo "release binaries in $(DIST)/"

# ---- the container ---------------------------------------------------------
#
# Everything that runs, runs in here. A test that resolves a home directory the
# wrong way finds the container's, and this repository has written to a real
# ~/.ssh by getting that wrong once.
#
# Two things cannot come in and stay where they are: `make app`, which needs
# codesign, lipo and iconutil, and anything iOS, which needs Xcode. Both are
# macOS, and macOS does not containerise.

IMAGE := forge-build
DOCKER := docker

# The host's own uid, so files the container writes into the mounted tree belong
# to whoever ran make rather than to root.
UID := $(shell id -u)
GID := $(shell id -g)

# A named volume for the module cache: without it every run downloads the world,
# and a bind mount of ~/go would be the container reaching back onto the machine
# this exists to stay off.
RUN := $(DOCKER) run --rm -u $(UID):$(GID) 	-v "$(CURDIR)":/src -v forge-gomod:/go/pkg/mod -e GOFLAGS=-buildvcs=false 	-w /src

image:
	$(DOCKER) build --target test -t $(IMAGE) -f build/Dockerfile .

image-android:
	$(DOCKER) build --target android -t $(IMAGE)-android -f build/Dockerfile .

# The suite, where it belongs. Same three -dev packages CI installs, so a pass
# here means the same thing as a pass there.
docker-test: image
	$(RUN) $(IMAGE) go test ./... -count=1

# A prompt inside it, for when something needs looking at rather than running.
shell: image
	$(DOCKER) run --rm -it -u $(UID):$(GID) 		-v "$(CURDIR)":/src -v forge-gomod:/go/pkg/mod -w /src $(IMAGE) bash

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
