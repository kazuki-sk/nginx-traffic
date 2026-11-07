VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS    ?= linux
GOARCH  ?= $(shell go env GOARCH 2>/dev/null || echo amd64)
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/nginx-sitemap
PKG     := nginx-sitemap-$(VERSION)-$(GOOS)-$(GOARCH)
STAGE   := dist/$(PKG)
CROSSPLANE_DIR = $(shell go list -m -f '{{.Dir}}' github.com/nginxinc/nginx-go-crossplane)

.PHONY: build test golden verify-fixtures stage dist install clean

# ---- on a build machine (needs Go) ----

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/nginx-sitemap

test:
	go test ./...

# Regenerate golden files and the JSON schema after reviewing a change.
golden:
	go test ./internal/model -update
	go test ./internal/app -update

# Validate fixtures with the real nginx in Docker (nginx -t).
verify-fixtures:
	scripts/verify-fixtures.sh nginx:1.30.0

# Lay out the distribution directory (binary + units + samples + install.sh).
stage: build
	rm -rf $(STAGE)
	install -D -m 0755 $(BIN)                                         $(STAGE)/nginx-sitemap
	install -D -m 0755 scripts/install.sh                             $(STAGE)/install.sh
	install -D -m 0644 deploy/systemd/nginx-sitemap.service           $(STAGE)/systemd/nginx-sitemap.service
	install -D -m 0644 deploy/systemd/nginx-sitemap.timer             $(STAGE)/systemd/nginx-sitemap.timer
	install -D -m 0644 deploy/systemd/nginx-sitemap.sysusers.conf     $(STAGE)/systemd/nginx-sitemap.sysusers.conf
	install -D -m 0644 deploy/systemd/nginx-sitemap.service.d/resolve.conf.example $(STAGE)/systemd/nginx-sitemap.service.d/resolve.conf.example
	install -D -m 0644 deploy/sitemap.conf.example                    $(STAGE)/sitemap.conf.example
	install -D -m 0644 deploy/nginx/nginx-sitemap.conf                $(STAGE)/nginx/nginx-sitemap.conf
	install -D -m 0644 README.md                                      $(STAGE)/README.md
	install -D -m 0644 docs/DESIGN.md                                 $(STAGE)/docs/DESIGN.md
	install -D -m 0644 docs/development.md                            $(STAGE)/docs/development.md
	install -D -m 0644 docs/spec-basis.md                             $(STAGE)/docs/spec-basis.md
	install -D -m 0644 schema/map.schema.json                         $(STAGE)/schema/map.schema.json
	install -D -m 0644 $(CROSSPLANE_DIR)/LICENSE                      $(STAGE)/LICENSES/nginx-go-crossplane.LICENSE
	echo "$(VERSION) $(GOOS)/$(GOARCH) $$(go env GOVERSION)" > $(STAGE)/VERSION

# Build the distributable tarball: dist/nginx-sitemap-<ver>-linux-<arch>.tar.gz
# Install on the target with:  tar xzf ...; cd ...; sudo ./install.sh   (no Go, no make)
dist: stage
	tar -C dist --owner=0 --group=0 --numeric-owner -czf dist/$(PKG).tar.gz $(PKG)
	cd dist && sha256sum $(PKG).tar.gz > $(PKG).tar.gz.sha256
	@echo "dist/$(PKG).tar.gz"

# Install from a source checkout. Uses an already built bin/nginx-sitemap
# (run `make build` first, possibly on another machine); does not build.
install:
	@test -x $(BIN) || { echo "$(BIN) not found: run 'make build' first (needs Go), or install from a release tarball" >&2; exit 1; }
	./scripts/install.sh --from-source

clean:
	rm -rf bin dist
