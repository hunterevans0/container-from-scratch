ROOTFS ?= /var/lib/runt/rootfs
# User whose /etc/subuid and /etc/subgid ranges the container maps onto.
# Empty means the user running sudo.
USERNS_USER ?=

.PHONY: build test vet hello rootfs apparmor shell clean

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
	rm -f runt
