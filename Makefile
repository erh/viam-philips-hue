
GO_BUILD_ENV :=
GO_BUILD_FLAGS :=
MODULE_BINARY := bin/viam-philips-hue

ifeq ($(VIAM_TARGET_OS), windows)
	GO_BUILD_ENV += GOOS=windows GOARCH=amd64
	GO_BUILD_FLAGS := -tags no_cgo
	MODULE_BINARY = bin/viam-philips-hue.exe
endif

$(MODULE_BINARY): Makefile go.mod *.go cmd/module/*.go
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MODULE_BINARY) cmd/module/cmd.go

lint:
	gofmt -s -w .

update:
	go get go.viam.com/rdk@latest
	go mod tidy

test:
	go test ./...

module.tar.gz: meta.json $(MODULE_BINARY)
	tar czf $@ meta.json $(MODULE_BINARY)
	git checkout meta.json

ifeq ($(VIAM_TARGET_OS), windows)
module.tar.gz: fix-meta-for-win
else
module.tar.gz: strip-module
endif

strip-module: $(MODULE_BINARY)
	strip $(MODULE_BINARY)

fix-meta-for-win:
	jq '.entrypoint = "./bin/viam-philips-hue.exe"' meta.json > temp.json && mv temp.json meta.json

module: test module.tar.gz

all: test $(MODULE_BINARY) module

setup:
