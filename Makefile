BINARY    := continuum-relay
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

TARGETS := $(foreach p,$(PLATFORMS),$(BINARY)-$(subst /,-,$(p)))

# Reproducible release builds:
#   -trimpath          strips local filesystem paths from the binary
#   -ldflags="-s -w"   strips DWARF debug + symbol tables (~30% smaller)
#   CGO_ENABLED=0      static binary, no glibc/libSystem dependency
GO_LDFLAGS := -s -w
GO_BUILD   := go build -trimpath -ldflags="$(GO_LDFLAGS)"

.PHONY: all clean checksums release

all: $(TARGETS)

$(BINARY)-linux-amd64:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 $(GO_BUILD) -o $@ .

$(BINARY)-linux-arm64:
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 $(GO_BUILD) -o $@ .

$(BINARY)-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO_BUILD) -o $@ .

$(BINARY)-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO_BUILD) -o $@ .

# Regenerate checksums.txt for the four release binaries.
# Uses sha256sum on Linux, falls back to `shasum -a 256` on macOS.
checksums: $(TARGETS)
	@if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum $(TARGETS) > checksums.txt; \
	else \
		shasum -a 256 $(TARGETS) > checksums.txt; \
	fi
	@cat checksums.txt

# Build everything fresh and emit checksums in one go.
release: clean checksums

clean:
	rm -f $(TARGETS) checksums.txt
