# Service-level Makefile.
SERVICE  := backup
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)
LDFLAGS  := -s -w \
            -X main.version=$(VERSION) \
            -X main.commit=$(COMMIT) \
            -X main.revision=$(REVISION)

.PHONY: build test fmt vet lint run docker tidy swag clean

build: ui
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(SERVICE) ./cmd/backup

ui:
	# `npm ci` is delete-and-reinstall by design — that is the whole
	# point: the resulting node_modules is what the lockfile pins, no
	# more and no less. A presence check that skipped it when
	# node_modules already existed (previously on this line) defeated
	# that guarantee: a stale node_modules survived every `make ui`
	# and `npm run build:prod` embedded whatever was on disk. CI calls
	# npm ci directly and the Dockerfile runs it in a clean layer, so
	# the shipped artifact was always honest; a local `make build`
	# before a manual smoke was the only path it mis-served on, but
	# the same shape as #45's stale-lockfile bug a layer down.
	cd internal/ui && npm ci --no-audit --no-fund
	cd internal/ui && npm run build:prod

test:
	go test -race -count=1 ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

lint: fmt vet

run: build
	SERVICE_JWT_SECRET=$${SERVICE_JWT_SECRET:-development-key-at-least-32-bytes-long-abc} \
	MONGO_URI=$${MONGO_URI:-mongodb://localhost:27017/sc_component_backup} \
	PORT=$${PORT:-8080} \
	./$(SERVICE)

docker:
	# Local build only. NO push — see root CLAUDE.md "no Dockerhub publish".
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg REVISION=$(REVISION) \
	  -t simplecontainer/sc-component-backup:local \
	  .

tidy:
	go mod tidy

swag:
	@echo "swagger regen stub — add swaggo/swag invocation once annotations land in handlers"

clean:
	rm -f $(SERVICE)
	rm -rf internal/ui/dist/* internal/ui/node_modules
