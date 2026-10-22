# Build 2o for the desktop and bundle the Linux server binaries it uploads.
#
#   make            → build/2o (this machine) + build/2o-linux-amd64 + build/2o-linux-arm64
#   make install    → ~/.local/bin/2o and ~/.local/lib/2o/2o-linux-{amd64,arm64}
#                     (PREFIX=/usr/local, or DESTDIR for packaging)
#
# `2o ssh` uploads to a server the binary matching the server's CPU: itself
# when the architectures match, otherwise the bundled 2o-linux-<arch> found
# next to it (same dir, or ../lib/2o/). No Go is needed on either side.

PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
LIBDIR  ?= $(PREFIX)/lib/2o
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOFLAGS_REL := -trimpath
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILT)
export CGO_ENABLED := 0

.PHONY: all desktop servers install uninstall test clean
all: desktop servers

desktop:
	@mkdir -p build
	go build $(GOFLAGS_REL) -ldflags '$(LDFLAGS)' -o build/2o .

servers: $(ARCHES:%=build/2o-linux-%)

build/2o-linux-%: FORCE
	@mkdir -p build
	GOOS=linux GOARCH=$* go build $(GOFLAGS_REL) -ldflags '$(LDFLAGS)' -o $@ .

FORCE:

install: all
	install -Dm755 build/2o $(DESTDIR)$(BINDIR)/2o
	for a in $(ARCHES); do install -Dm755 build/2o-linux-$$a $(DESTDIR)$(LIBDIR)/2o-linux-$$a; done

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/2o
	for a in $(ARCHES); do rm -f $(DESTDIR)$(LIBDIR)/2o-linux-$$a; done
	-rmdir $(DESTDIR)$(LIBDIR) 2>/dev/null

test:
	go test ./...

clean:
	rm -rf build
