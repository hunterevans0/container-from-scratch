ROOTFS ?= /var/lib/runt/rootfs
# User whose /etc/subuid and /etc/subgid ranges the container maps onto.
# Empty means the user running sudo.
USERNS_USER ?=

.PHONY: build build-debug test vet hello rootfs apparmor shell debug debug-init debug-attach-init clean

# CGO_ENABLED=0: dropping capabilities uses syscall.AllThreadsSyscall, which
# does not work in a cgo binary.
build:
	CGO_ENABLED=0 go build -o runt ./cmd/runt

test:
	go test ./...

vet:
	go vet ./...

hello:
	go run ./cmd/runt hello

rootfs:
	sudo USERNS_USER=$(USERNS_USER) scripts/make-rootfs.sh $(ROOTFS)

# Load the AppArmor profile into the kernel, for --apparmor-profile runt-default.
apparmor:
	sudo apparmor_parser --replace apparmor/runt-default

shell: build
	sudo ./runt run --rootfs $(ROOTFS) $(if $(USERNS_USER),--userns-user $(USERNS_USER)) -- /bin/bash

clean:
	rm -f runt runt-debug

# Debugging. Delve runs as root inside WSL and listens on localhost, where VS
# Code on Windows connects to it (see .vscode/launch.json). Delve is installed
# with: go install github.com/go-delve/delve/cmd/dlv@latest
DLV ?= $(shell go env GOPATH)/bin/dlv
DEBUG_PORT ?= 2345
INIT_DEBUG_PORT ?= 2346
RUN_ARGS = --rootfs $(ROOTFS) $(if $(USERNS_USER),--userns-user $(USERNS_USER))
# --only-same-user=false: connections forwarded from Windows don't come from
# the user running Delve (root), so Delve would refuse them.
DLV_SERVER = --headless --api-version=2 --accept-multiclient --only-same-user=false

# -N -l turns off optimizations and inlining so every variable and line can be
# inspected.
build-debug:
	CGO_ENABLED=0 go build -gcflags='all=-N -l' -o runt-debug ./cmd/runt

# Debug the runt parent process (Run). Breakpoints in Init won't hit here,
# because init is a separate process; use debug-init for that.
debug: build-debug
	sudo $(DLV) exec $(DLV_SERVER) --listen=127.0.0.1:$(DEBUG_PORT) ./runt-debug -- run $(RUN_ARGS) -- /bin/bash

# Start a container whose init process waits for a debugger, then attach with
# debug-attach-init from a second terminal.
debug-init: build-debug
	sudo ./runt-debug run --debug-init $(RUN_ARGS) -- /bin/bash

debug-attach-init:
	@echo "waiting for a container started with make debug-init..."; \
	until pid=$$(pgrep -n -f '^/proc/self/exe --init'); do sleep 0.5; done; \
	echo "attaching to container init, host PID $$pid"; \
	sudo $(DLV) attach $(DLV_SERVER) --listen=127.0.0.1:$(INIT_DEBUG_PORT) $$pid
