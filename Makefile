SHELL := /usr/bin/env bash

.PHONY: fmt test vet build check deploy

fmt:
	gofmt -w .

test:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -buildvcs=false -o bin/jeff ./cmd/jeff

check: fmt test vet build

deploy: build
	@sudo -n /usr/bin/systemctl restart jeff.service
