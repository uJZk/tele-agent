GO            ?= go
CC            ?= cc
GOARCH        ?= $(shell $(GO) env GOARCH)
GOLANGCI_LINT ?= $(GO) tool -modfile=tools/go.mod golangci-lint

BIN           := bin/tele
# Release builds stamp the version; without git it stays empty and tele
# reports the VCS revision the Go toolchain recorded instead.
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS       := -s -w -X github.com/ujzk/tele-agent/internal/version.Version=$(VERSION)
TELESWITCH_SO := internal/teleswitch/lib/teleswitch-$(GOARCH).so
# teleswitch runs inside the Claude process before main: no libc, no
# DT_NEEDED, raw syscalls only (docs/coding-standards.md "C 代码（teleswitch）").
TELESWITCH_CFLAGS := -std=c11 -O2 -fPIC -shared -nostdlib -ffreestanding \
	-fno-stack-protector -fno-builtin -Wall -Wextra -Werror -Wl,-z,now

.PHONY: all build teleswitch test test-priv lint fmt check clean

all: build

teleswitch: $(TELESWITCH_SO)

$(TELESWITCH_SO): internal/teleswitch/csrc/teleswitch.c
	$(CC) $(TELESWITCH_CFLAGS) -o $@ $<

build: teleswitch
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/tele

test:
	$(GO) test -race ./...

test-priv:
	TELE_TEST_REQUIRE_PRIV=1 $(GO) test -race -count=1 ./...

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

check: lint test

clean:
	rm -rf bin $(wildcard internal/teleswitch/lib/*.so)
