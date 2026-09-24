ROOTFS ?= /var/lib/runt/rootfs

.PHONY: build test vet hello rootfs shell clean

build:
	go build -o runt ./cmd/runt

test:
	go test ./...

vet:
	go vet ./...

hello:
	go run ./cmd/runt hello

rootfs:
	sudo scripts/make-rootfs.sh $(ROOTFS)

shell: build
	sudo ./runt run --rootfs $(ROOTFS) -- /bin/bash

clean:
	rm -f runt
