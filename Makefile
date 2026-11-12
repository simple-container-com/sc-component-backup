# Top-level Makefile for sc-component-backup.
#
# Fans out to the three Go modules. Each module has its own Makefile for
# per-module targets; this file just drives the whole repo.

.PHONY: build test tidy fmt vet lint clean swag docker-service help

help:
	@echo "Targets:"
	@echo "  build          Build plugin + service binaries"
	@echo "  test           Run tests in all three modules"
	@echo "  tidy           go mod tidy in all three modules"
	@echo "  fmt            gofmt -w across all three modules"
	@echo "  vet            go vet across all three modules"
	@echo "  lint           Run gci + gofumpt + go vet"
	@echo "  swag           Regenerate service swagger (requires swaggo)"
	@echo "  docker-service Build the service Docker image locally (no push)"
	@echo "  clean          Remove built binaries + dist dirs"

build:
	cd plugin && go build -o ../sc-component-backup -ldflags "-s -w" ./cmd/sc-component-backup
	cd service && $(MAKE) build

test:
	cd api && go test ./...
	cd plugin && go test ./...
	cd service && go test ./...

tidy:
	cd api && go mod tidy
	cd plugin && go mod tidy
	cd service && go mod tidy

fmt:
	cd api && gofmt -w .
	cd plugin && gofmt -w .
	cd service && gofmt -w .

vet:
	cd api && go vet ./...
	cd plugin && go vet ./...
	cd service && go vet ./...

lint: fmt vet

swag:
	cd service && $(MAKE) swag

docker-service:
	cd service && $(MAKE) docker

clean:
	rm -f sc-component-backup service/backup
	rm -rf dist/ service/internal/ui/dist/
