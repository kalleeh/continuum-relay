BINARY   := continuum-relay
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

TARGETS := $(foreach p,$(PLATFORMS),$(BINARY)-$(subst /,-,$(p)))

.PHONY: all clean

all: $(TARGETS)

$(BINARY)-linux-amd64:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o $@ .

$(BINARY)-linux-arm64:
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o $@ .

$(BINARY)-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -o $@ .

$(BINARY)-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o $@ .

clean:
	rm -f $(TARGETS)
