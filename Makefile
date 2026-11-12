VERSION ?= 0.5.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test clean

build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/plex-artwork-sync-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/plex-artwork-sync-linux-arm64 .

test:
	go vet ./...
	go test -count=1 ./...

clean:
	rm -rf dist
