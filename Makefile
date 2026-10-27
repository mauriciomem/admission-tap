# admission-tap: build, test and publish the container image.
# Deploying to a cluster is done with scripts/deploy.sh (see the README).

IMG      ?= admission-tap:dev
PLATFORM ?= linux/amd64
# Prefer docker if installed, else podman.
CONTAINER ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)

.DEFAULT_GOAL := help
.PHONY: help test build image push

help:
	@echo "Targets:"
	@echo "  test   go vet + go test -race"
	@echo "  build  build a local binary to bin/admission-tap"
	@echo "  image  build the container image (IMG, PLATFORM)"
	@echo "  push   push the container image (IMG)"
	@echo
	@echo "Variables:"
	@echo "  IMG        image reference          (default: $(IMG))"
	@echo "  PLATFORM   target platform          (default: $(PLATFORM))"
	@echo "  CONTAINER  docker or podman         (default: $(CONTAINER))"

test:
	go vet ./...
	go test -race ./...

build:
	CGO_ENABLED=0 go build -o bin/admission-tap .

image:
	$(CONTAINER) build --platform $(PLATFORM) -t $(IMG) .

push:
	$(CONTAINER) push $(IMG)
