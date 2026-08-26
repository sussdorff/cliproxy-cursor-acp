.PHONY: test test-race vet build build-linux release-archive clean

VERSION ?= $(shell awk -F'"' '/"version"/{print $$4; exit}' plugin-store/registry.json)
GO_VERSION ?= $(shell awk '/^go /{print $$2; exit}' go.mod)
# Keep this digest synchronized with .github/workflows/release.yml.
GOLANG_IMAGE := golang@sha256:fb612b7831d53a89cbc0aaa7855b69ad7b0caf603715860cf538df854d047b84
PLUGIN_ID := cliproxy-cursor-acp
ARCHIVE := $(PLUGIN_ID)_$(VERSION)_linux_amd64.zip

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p build
	go build -buildmode=c-shared -o build/$(PLUGIN_ID).so ./cmd/$(PLUGIN_ID)

# build-linux reproduces the release build inside the same golang image the
# release workflow uses, so the CGO shared library matches the published asset.
build-linux:
	mkdir -p build
	docker run --rm -v "$(CURDIR)":/src -w /src \
		-e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-buildvcs=false \
		$(GOLANG_IMAGE) \
		go build -buildmode=c-shared -trimpath -o build/$(PLUGIN_ID).so ./cmd/$(PLUGIN_ID)

# release-archive produces the exact plugin-store asset names from an existing
# build/$(PLUGIN_ID).so, so the naming contract can be checked locally.
release-archive:
	test -f build/$(PLUGIN_ID).so
	rm -rf dist
	mkdir -p dist
	cp build/$(PLUGIN_ID).so dist/$(PLUGIN_ID).so
	cd dist && zip -X -j $(ARCHIVE) $(PLUGIN_ID).so
	rm dist/$(PLUGIN_ID).so
	cd dist && shasum -a 256 $(ARCHIVE) >checksums.txt
	cd dist && unzip -l $(ARCHIVE)

clean:
	rm -rf build dist
