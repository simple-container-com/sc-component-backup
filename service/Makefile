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
	@if [ -d internal/ui/node_modules ]; then \
	  echo "ui/node_modules present — skipping npm install"; \
	else \
	  cd internal/ui && npm install --no-audit --no-fund; \
	fi
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
