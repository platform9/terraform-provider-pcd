GOBIN ?= $(shell go env GOPATH)/bin

default: build

# Build all packages.
build:
	go build ./...

# Install the provider binary into GOBIN (for ~/.terraformrc dev_overrides).
install:
	go install ./...

# Unit tests only — no lab required. Live/acceptance tests are gated by env.
test:
	go test ./internal/... -timeout 120s

# Acceptance tests against a real PCD lab. Requires OS_* env (see docs) and terraform.
testacc:
	TF_ACC=1 go test ./internal/... -v -timeout 120m

# Static analysis.
lint:
	golangci-lint run ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

# Regenerate registry docs from schema descriptions + examples (tfplugindocs).
generate:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name pcd

# Remove lab resources left by failed acceptance runs (tf-acc- prefix). Added per family.
sweep:
	TF_ACC=1 go test ./internal/... -v -sweep=all -timeout 60m

.PHONY: default build install test testacc lint vet fmt generate sweep
