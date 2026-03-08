PREFIX  ?= /usr/local
VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)
BIN = bin/sigr

fmt:
	gofmt -w .

check-fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l . ; echo "Run 'make fmt' to fix formatting"; exit 1; }

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: build
	install -d $(PREFIX)/bin
	install -m 755 $(BIN) $(PREFIX)/bin/sigr

reinstall: uninstall install

uninstall:
	rm -f $(PREFIX)/bin/sigr

release:
ifndef v
	$(error usage: make release v=0.1.0)
endif
	gh workflow run release.yml -f version=$(v)

clean:
	rm -f $(BIN)

.PHONY: fmt check-fmt build install reinstall uninstall clean release
