PLUGIN_ID := prompt-admin
VERSION ?= 0.1.1
DIST := dist
LDFLAGS := -s -w -X cpa-plugin-prompt-admin/internal/runtime.Version=$(VERSION)

# Darwin cgo links CoreFoundation, Security, and libresolv. Zig does not ship those.
# The default SDK is fetched by the macos-sdk target. Set MACOSX_SDK to use another tree.
# The SDK is Apple's; accept the Xcode license terms before using it.
# Cross compilers. Override on the command line, for example
#   make release-nodarwin CC_LINUX_ARM64="zig cc -target aarch64-linux-gnu.2.17" CC_WINDOWS_AMD64="zig cc -target x86_64-windows-gnu"
CC_LINUX_AMD64 ?= gcc
CC_LINUX_ARM64 ?= aarch64-linux-gnu-gcc
CC_WINDOWS_AMD64 ?= x86_64-w64-mingw32-gcc

MACOSX_SDK ?= $(CURDIR)/.cache/MacOSX14.5.sdk
MACOSX_SDK_URL := https://github.com/joseluisq/macosx-sdks/releases/download/14.5/MacOSX14.5.sdk.tar.xz
DARWIN_CGO_LDFLAGS := -mmacosx-version-min=11.3 -F$(MACOSX_SDK)/System/Library/Frameworks -L$(MACOSX_SDK)/usr/lib
DARWIN_CC_COMMON := -isysroot $(MACOSX_SDK) -iwithsysroot /usr/include -iframeworkwithsysroot /System/Library/Frameworks -F$(MACOSX_SDK)/System/Library/Frameworks -L$(MACOSX_SDK)/usr/lib

.PHONY: fmt fmt-check test build release release-nodarwin release-clean release-other macos-sdk checksums clean

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './dist/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './dist/*'))" || { gofmt -l $$(find . -name '*.go' -not -path './dist/*'); exit 1; }

test:
	env -u GOOS -u GOARCH -u CC -u CGO_LDFLAGS CGO_ENABLED=0 go test ./internal/...

# Linux amd64 library named for the plugin id. CPA loads plugins/<os>/<arch>/$(PLUGIN_ID).so.
build:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags="$(LDFLAGS)" -o $(DIST)/$(PLUGIN_ID).so .

# Plugin Store archives: $(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip
# Each zip contains only the library at the archive root. checksums.txt uses sha256sum layout.
release: test macos-sdk release-clean release-other
	$(MAKE) build-target GOOS=darwin GOARCH=amd64 CC="zig cc -target x86_64-macos $(DARWIN_CC_COMMON)" CGO_LDFLAGS="$(DARWIN_CGO_LDFLAGS)" LIB=$(PLUGIN_ID).dylib
	$(MAKE) build-target GOOS=darwin GOARCH=arm64 CC="zig cc -target aarch64-macos $(DARWIN_CC_COMMON)" CGO_LDFLAGS="$(DARWIN_CGO_LDFLAGS)" LIB=$(PLUGIN_ID).dylib
	$(MAKE) checksums

# Linux and Windows archives only (no Apple SDK needed).
release-nodarwin: test release-clean release-other
	$(MAKE) checksums

release-clean:
	rm -rf $(DIST)/release $(DIST)/checksums.txt $(DIST)/$(PLUGIN_ID)_$(VERSION)_*.zip
	mkdir -p $(DIST)/release

release-other:
	$(MAKE) build-target GOOS=linux GOARCH=amd64 CC="$(CC_LINUX_AMD64)" LIB=$(PLUGIN_ID).so
	$(MAKE) build-target GOOS=linux GOARCH=arm64 CC="$(CC_LINUX_ARM64)" LIB=$(PLUGIN_ID).so
	$(MAKE) build-target GOOS=windows GOARCH=amd64 CC="$(CC_WINDOWS_AMD64)" LIB=$(PLUGIN_ID).dll

checksums:
	cd $(DIST)/release && sha256sum $(PLUGIN_ID)_$(VERSION)_*.zip > checksums.txt
	cp $(DIST)/release/checksums.txt $(DIST)/checksums.txt
	cp $(DIST)/release/$(PLUGIN_ID)_$(VERSION)_*.zip $(DIST)/

macos-sdk:
	@if [ ! -d "$(MACOSX_SDK)/usr/include" ]; then \
		echo "downloading MacOSX 14.5 SDK into $(MACOSX_SDK)"; \
		echo "this SDK is Apple's; accept the Xcode license terms before using it"; \
		mkdir -p "$(dir $(MACOSX_SDK))"; \
		curl -fL --retry 3 -o "$(MACOSX_SDK).tar.xz" "$(MACOSX_SDK_URL)"; \
		tar -C "$(dir $(MACOSX_SDK))" -xf "$(MACOSX_SDK).tar.xz"; \
	fi
	@test -d "$(MACOSX_SDK)/usr/include"

build-target:
	@command -v $(firstword $(CC)) >/dev/null || { echo "missing cross compiler: $(CC)"; exit 1; }
	rm -rf $(DIST)/release/obj
	mkdir -p $(DIST)/release/obj
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) CC="$(CC)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags="$(LDFLAGS)" -o $(DIST)/release/obj/$(LIB) .
	rm -f $(DIST)/release/obj/$(PLUGIN_ID).h
	cd $(DIST)/release/obj && zip -q -X ../$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip $(LIB)
	@if [ "$(GOOS)" = "linux" ] && [ "$(GOARCH)" = "amd64" ]; then cp $(DIST)/release/obj/$(LIB) $(DIST)/$(PLUGIN_ID).so; fi

clean:
	rm -rf $(DIST)
	go clean
