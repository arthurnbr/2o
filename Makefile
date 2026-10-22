# Build 2o for the desktop and bundle the Linux server binaries it uploads.
#
#   make            → build/2o (this machine) + build/2o-linux-amd64 + build/2o-linux-arm64
#   make install    → ~/.local/bin/2o and ~/.local/lib/2o/2o-linux-{amd64,arm64}
#                     (PREFIX=/usr/local, or DESTDIR for packaging)
#   make dist       → dist/: release tarballs (linux, darwin), .deb, .rpm, SHA256SUMS
#
# `2o ssh` uploads to a server the binary matching the server's CPU: itself
# when the architectures match, otherwise the bundled 2o-linux-<arch> found
# next to it (same dir, or ../lib/2o/). No Go is needed on either side.

PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
LIBDIR  ?= $(PREFIX)/lib/2o
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCHES  := amd64 arm64
# The build time orders builds for `2o ssh` updates, so every build of one
# source must carry the same one: SOURCE_DATE_EPOCH (packagers), else the
# commit time of a clean checkout, else now (uncommitted changes).
ifndef SOURCE_DATE_EPOCH
SOURCE_DATE_EPOCH := $(shell git diff --quiet HEAD 2>/dev/null && git log -1 --format=%ct 2>/dev/null)
endif
ifneq ($(SOURCE_DATE_EPOCH),)
BUILT   ?= $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)
else
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
endif

GOFLAGS_REL := -trimpath
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILT)
export CGO_ENABLED := 0

.PHONY: all desktop servers install uninstall test clean dist
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
	rm -rf build dist

# Release assets. Every desktop tarball carries the Linux server binaries.
DIST_TARGETS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64
NFPM         := go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0
PKGVERSION    = $(patsubst v%,%,$(VERSION))

dist: servers
	rm -rf dist && mkdir -p dist
	set -e; for t in $(DIST_TARGETS); do \
	  d=dist/2o_$(PKGVERSION)_$${t%-*}_$${t#*-}; mkdir -p $$d; \
	  GOOS=$${t%-*} GOARCH=$${t#*-} go build $(GOFLAGS_REL) -ldflags '$(LDFLAGS)' -o $$d/2o .; \
	  cp build/2o-linux-amd64 build/2o-linux-arm64 README.md $$d/; \
	  tar -C dist --owner=0 --group=0 --numeric-owner $(if $(SOURCE_DATE_EPOCH),--mtime=@$(SOURCE_DATE_EPOCH)) -czf $$d.tar.gz $${d#dist/}; \
	  rm -rf $$d; \
	done
	set -e; for a in $(ARCHES); do cp build/2o-linux-$$a build/2o-pkg; for p in deb rpm; do \
	  VERSION=$(PKGVERSION) PKGARCH=$$a $(NFPM) package -f packaging/nfpm.yaml -p $$p -t dist/; \
	done; done; rm -f build/2o-pkg
	cd dist && sha256sum * > SHA256SUMS
