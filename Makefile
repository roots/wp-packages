.PHONY: build install dev test integration lint clean tailwind db-restore venv go.mod

VAULT_FILE ?= deploy/ansible/group_vars/production/vault.yml

TAILWIND ?= ./bin/tailwindcss
ANSIBLE_VENV ?= deploy/ansible/.venv

# Download Tailwind standalone CLI if missing
tailwind-install:
	@if [ ! -f $(TAILWIND) ]; then \
		mkdir -p bin; \
		ARCH=$$(uname -m); \
		OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
		case "$$OS" in darwin) OS=macos ;; esac; \
		case "$$ARCH" in \
			x86_64) ARCH=x64 ;; \
			aarch64|arm64) ARCH=arm64 ;; \
		esac; \
		echo "Downloading tailwindcss for $$OS-$$ARCH..."; \
		curl -sLo $(TAILWIND) "https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-$$OS-$$ARCH"; \
		chmod +x $(TAILWIND); \
	fi

# Generate CSS from templates
tailwind: tailwind-install
	$(TAILWIND) -i internal/http/tailwind-input.css -o internal/http/static/assets/styles/app.css --minify

# Build the binary
build: tailwind
	go build -o wppackages ./cmd/wppackages

# Install to $GOPATH/bin
install:
	go install ./cmd/wppackages

# One-time setup: migrate, create admin, seed packages
dev-bootstrap: build
	./wppackages migrate
	echo admin | ./wppackages admin create --email admin@localhost --name Admin --password-stdin
	./wppackages discover --source config
	./wppackages update --force

# Live-reload dev server (rebuild binary + serve on file changes)
dev: tailwind-install
	air

# Run all tests
test:
	go test ./...

# Integration tests (requires composer)
integration:
	go test -tags=integration -count=1 -timeout=5m -v ./internal/integration/...

# Lint (matches CI: golangci-lint + go mod tidy)
lint:
	$(shell go env GOPATH)/bin/golangci-lint run ./...
	go mod tidy -diff

# Create Ansible virtualenv if missing
venv:
	@if [ ! -f $(ANSIBLE_VENV)/bin/ansible-vault ]; then \
		echo "Creating Ansible virtualenv..."; \
		python3 -m venv $(ANSIBLE_VENV); \
		$(ANSIBLE_VENV)/bin/pip install -q -r deploy/ansible/requirements.txt; \
	fi

# Restore production database from R2 (reads secrets from Ansible vault)
db-restore: venv
	@eval $$($(ANSIBLE_VENV)/bin/ansible-vault view --vault-password-file deploy/ansible/.vault_pass $(VAULT_FILE) | yq -r \
		'"export LITESTREAM_BUCKET=\(.vault_r2_litestream_bucket) R2_ENDPOINT=\(.vault_r2_endpoint) R2_ACCESS_KEY_ID=\(.vault_r2_access_key_id) R2_SECRET_ACCESS_KEY=\(.vault_r2_secret_access_key)"') && \
		export DB_PATH=./storage/wppackages.db && \
		echo "LITESTREAM_BUCKET=$$LITESTREAM_BUCKET" && \
		echo "R2_ENDPOINT=$$R2_ENDPOINT" && \
		echo "R2_ACCESS_KEY_ID=$$R2_ACCESS_KEY_ID" && \
		echo "R2_SECRET_ACCESS_KEY=$$R2_SECRET_ACCESS_KEY" && \
		go run ./cmd/wppackages db restore --force

# Remove build artifacts
clean:
	rm -f wppackages
	rm -rf storage/

# Update Go modules
go.mod:
	go get -u -t -tags=integration,wporg_live ./...
	go mod tidy

go.sum: go.mod
