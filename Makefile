# Raven Partition Manager. `make`, `make run`, `sudo make install`. lazy.toml
# mirrors these for imlazy; keep the two in step.

APP_ID    := com.ravenpartitionmanager.Raven
BIN_NAME  := raven-partition-manager
PREFIX    ?= /usr/local
BINDIR    := $(PREFIX)/bin
DATADIR   := $(PREFIX)/share
ICONDIR   := $(DATADIR)/icons/hicolor/scalable/apps
DESTDIR   ?=
BUILD_DIR := bin

.PHONY: all deps build run probe test check clean install uninstall

# GTK 4 is linked through cgo by gotk4: the rvn package name, and the
# pkg-config module that shows it is there. Installed through rvn only when
# missing. parted, util-linux (lsblk, sfdisk, wipefs, partx) and polkit
# (pkexec) are needed at run time.
RVN_DEPS := gtk4
PC_DEPS  := gtk4 >= 4.12

all: build

deps:
	@pkg-config --exists '$(PC_DEPS)' || { \
		echo "Installing build dependencies with rvn: $(RVN_DEPS)"; \
		rvn install --repo-only -y $(RVN_DEPS); \
	}

build: deps
	go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/$(BIN_NAME) ./cmd/$(BIN_NAME)

run: build
	./$(BUILD_DIR)/$(BIN_NAME)

probe: build
	./$(BUILD_DIR)/$(BIN_NAME) --probe

test:
	go test ./internal/disk ./internal/ops

check: deps
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	go vet ./...
	go test ./...

clean:
	rm -rf $(BUILD_DIR)

# Refreshing the caches is what makes the entry show up in launchers; skipped
# under DESTDIR, where the packager runs them itself.
define update-caches
	@if [ -z "$(DESTDIR)" ]; then \
		command -v update-desktop-database >/dev/null 2>&1 && \
			update-desktop-database -q "$(DATADIR)/applications" || true; \
		command -v gtk-update-icon-cache >/dev/null 2>&1 && \
			gtk-update-icon-cache -qtf "$(DATADIR)/icons/hicolor" || true; \
	fi
endef

install: build
	install -Dm755 "$(BUILD_DIR)/$(BIN_NAME)" "$(DESTDIR)$(BINDIR)/$(BIN_NAME)"
	install -Dm644 "data/$(APP_ID).desktop" "$(DESTDIR)$(DATADIR)/applications/$(APP_ID).desktop"
	install -Dm644 "data/$(APP_ID).metainfo.xml" "$(DESTDIR)$(DATADIR)/metainfo/$(APP_ID).metainfo.xml"
	install -Dm644 "data/icons/hicolor/scalable/apps/$(APP_ID).svg" "$(DESTDIR)$(ICONDIR)/$(APP_ID).svg"
	$(update-caches)

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/$(BIN_NAME)"
	rm -f "$(DESTDIR)$(DATADIR)/applications/$(APP_ID).desktop"
	rm -f "$(DESTDIR)$(DATADIR)/metainfo/$(APP_ID).metainfo.xml"
	rm -f "$(DESTDIR)$(ICONDIR)/$(APP_ID).svg"
	$(update-caches)
