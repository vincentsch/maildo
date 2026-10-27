VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# Release tags may start with v; the version inside the binary never does.
BUILD_VERSION := $(patsubst v%,%,$(VERSION))
LDFLAGS := -s -w -X main.version=$(BUILD_VERSION) -X main.commit=$(COMMIT)
# Windows is not supported yet (maildo cannot make its saved logins private
# there), so it gets no release binary. `make lint` still checks that the
# code builds for it.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build build-all test race lint docs docs-check conformance integration mailservers-up mailservers-down release-check release-notes docs-links clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/maildo ./cmd/maildo

build-all:
	mkdir -p dist
	rm -f dist/maildo-* dist/checksums.txt
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		asset=maildo-$$os-$$arch; \
		echo "Building $$asset"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "dist/$$asset" ./cmd/maildo; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum maildo-* > checksums.txt; \
	else shasum -a 256 maildo-* > checksums.txt; fi

test:
	go test ./...

race:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }
	go vet ./...
	go vet -tags integration ./internal/integration
	GOOS=windows go vet ./...

# Regenerate docs/commands and the help goldens after changing commands or flags.
docs:
	MAILDO_UPDATE_GOLDENS=1 go test ./cmd/maildo -run TestGeneratedDocsAndHelpGoldens -count=1

docs-check:
	go test ./cmd/maildo -run TestGeneratedDocsAndHelpGoldens -count=1

conformance:
	go test -tags conformance ./cmd/maildo -run '^TestConformance|^TestOfflineFourProviderJourneys$$' -count=1

# Real-server tests against Dovecot and GreenMail in Docker.
mailservers-up:
	./scripts/mailservers.sh up

mailservers-down:
	./scripts/mailservers.sh down

integration:
	./scripts/mailservers.sh test

# The checks a release candidate must pass; RELEASE.md has the rest.
release-check: test race lint docs-check docs-links conformance
	git diff --check

docs-links:
	python3 scripts/docs-links.py

# Print the changelog section for VERSION, as the release workflow does.
release-notes:
	@python3 scripts/release-notes.py $(BUILD_VERSION)

clean:
	rm -f bin/maildo dist/maildo-* dist/checksums.txt
