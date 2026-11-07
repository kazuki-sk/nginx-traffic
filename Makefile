VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/nginx-sitemap

.PHONY: build test golden verify-fixtures install clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/nginx-sitemap

test:
	go test ./...

# Regenerate golden files and the JSON schema after reviewing a change.
golden:
	go test ./internal/app -update
	go test ./internal/model -update

# Validate fixtures with the real nginx in Docker (nginx -t).
verify-fixtures:
	scripts/verify-fixtures.sh nginx:1.30.0

install: build
	install -D -m 0755 $(BIN) $(DESTDIR)/usr/local/bin/nginx-sitemap
	install -D -m 0644 deploy/systemd/nginx-sitemap.service $(DESTDIR)/etc/systemd/system/nginx-sitemap.service
	install -D -m 0644 deploy/systemd/nginx-sitemap.timer $(DESTDIR)/etc/systemd/system/nginx-sitemap.timer
	install -D -m 0644 deploy/systemd/nginx-sitemap.sysusers.conf $(DESTDIR)/usr/lib/sysusers.d/nginx-sitemap.conf
	test -e $(DESTDIR)/etc/nginx-sitemap/sitemap.conf || install -D -m 0644 deploy/sitemap.conf.example $(DESTDIR)/etc/nginx-sitemap/sitemap.conf

clean:
	rm -rf bin
