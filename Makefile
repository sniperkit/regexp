.PHONY: all test clean man glide fast release install

GO15VENDOREXPERIMENT=1

PROG_NAME := "regexp"

all: deps test build install version

build: deps
	@go build -ldflags "-X main.VERSION=`cat VERSION`" -o ./bin/$(PROG_NAME) ./cmd/$(PROG_NAME)/*.go

version: deps
	@which $(PROG_NAME)
	@$(PROG_NAME) --version

install: deps
	@go install -ldflags "-X main.VERSION=`cat VERSION`" ./cmd/$(PROG_NAME)
	@$(PROG_NAME) --version

fast: deps
	@go build -i -ldflags "-X main.VERSION=`cat VERSION`-dev" -o ./bin/$(PROG_NAME) ./cmd/$(PROG_NAME)/*.go
	@$(PROG_NAME) --version

deps:
	@glide install --strip-vendor

test:
	@go test ./pkg/inputs/
	@go test ./pkg/storage/

clean:
	@go clean
	@rm -fr ./bin
	@rm -fr ./dist

release: $(PROG_NAME)
	@git tag -a `cat VERSION`
	@git push origin `cat VERSION`
