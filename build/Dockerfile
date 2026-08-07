# The container everything is built and tested in.
#
# Not tidiness. A test that resolves a home directory the wrong way writes to a
# real one, and this repository has done that: a test in internal/agent read the
# passwd file instead of HOME and created ~/.ssh/authorized_keys on the
# developer's own Mac. In here it would have found the container's home, and the
# mistake would have cost nothing.
#
# Two stages, because the Android toolchain is gigabytes and the test loop should
# not carry them:
#
#   test     — Go, and what cmd/forge-app compiles against
#   android  — the above, plus the SDK, the NDK and a JDK
#
# The Ubuntu version and the three -dev packages are deliberately the same ones
# .github/workflows/ci.yml installs. A container that disagreed with CI about
# them would be a container that passes what CI fails, which is worse than not
# having one.

# ---- test -------------------------------------------------------------------

# Go comes from its own image rather than apt: Debian and Ubuntu ship whatever
# they ship, and this repository pins its toolchain in go.mod.
FROM golang:1.26 AS go

FROM ubuntu:24.04 AS test
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/go/bin:$PATH \
    GOPATH=/go \
    GOTOOLCHAIN=local

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git make \
      # What cmd/forge-app needs to compile at all: it is cgo over the system's
      # webview, and without these `go vet ./...` does not cover the whole tree.
      gcc libc6-dev pkg-config \
      libgtk-4-dev libwebkitgtk-6.0-dev libx11-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# ---- android ----------------------------------------------------------------

FROM test AS android

# 26.3.x is what Wails v3 asks for; a newer NDK changes the ABI of what it
# builds, so it is pinned rather than left to "latest".
ENV ANDROID_HOME=/opt/android-sdk \
    ANDROID_NDK_VERSION=26.3.11579264 \
    ANDROID_PLATFORM=android-34 \
    ANDROID_BUILD_TOOLS=34.0.0

RUN apt-get update && apt-get install -y --no-install-recommends \
      openjdk-17-jdk-headless unzip curl \
    && rm -rf /var/lib/apt/lists/*

# The command-line tools are the only part Google ships as a plain download; the
# rest is fetched by sdkmanager, which is why this is one layer and not five.
RUN set -eu; \
    mkdir -p "$ANDROID_HOME/cmdline-tools"; \
    curl -fsSL -o /tmp/tools.zip \
      https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip; \
    unzip -q /tmp/tools.zip -d "$ANDROID_HOME/cmdline-tools"; \
    mv "$ANDROID_HOME/cmdline-tools/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest"; \
    rm /tmp/tools.zip; \
    yes | "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --licenses >/dev/null; \
    "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" \
      "platform-tools" \
      "platforms;$ANDROID_PLATFORM" \
      "build-tools;$ANDROID_BUILD_TOOLS" \
      "ndk;$ANDROID_NDK_VERSION" >/dev/null

ENV ANDROID_NDK_HOME=$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION \
    PATH=$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools:$PATH

# wails3 generates the Android project — the Gradle scaffold, the NDK wiring —
# and then `task` drives it. It is a Go program, so it is installed here and
# never on anybody's machine.
RUN go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.3

WORKDIR /src
