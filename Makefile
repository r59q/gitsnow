.PHONY: build web-build test

build: web-build
	go build -o gitsnow ./cmd/gitsnow

web-build:
	npm --prefix web run build

test: web-build
	npm --prefix web test
	go test -race ./...
	go vet ./...
