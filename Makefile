.PHONY: build test run clean docker-build docker-run

# Binary name
BINARY=bin/server

# Build the binary
build:
	go build -ldflags="-w -s" -o $(BINARY) cmd/server/main.go

# Run tests
test:
	go test -v ./...

# Run with coverage
test-coverage:
	go test -cover ./...

# Run the server locally (requires MySQL)
run:
	MOCK_GATEWAY_ENABLED=true \
	MOCK_GATEWAY_WEBHOOK_SECRET=dev-secret-min-32-characters-long \
	ADMIN_HMAC_SECRET=admin-secret-min-32-characters-long \
	DB_DSN="tatendak:password@tcp(localhost:19200)/radius?parseTime=true" \
	go run cmd/server/main.go

# Clean build artifacts
clean:
	rm -rf bin/

# Build Docker image
docker-build:
	docker build -f ../configs/docker-conf/payments-api/Dockerfile -t payments-api:latest ..

# Run Docker container
docker-run:
	docker run -p 19207:8080 \
		-e MOCK_GATEWAY_ENABLED=true \
		-e MOCK_GATEWAY_WEBHOOK_SECRET=dev-secret-min-32-characters-long \
		-e ADMIN_HMAC_SECRET=admin-secret-min-32-characters-long \
		-e DB_DSN="tatendak:password@tcp(host.docker.internal:19200)/radius?parseTime=true" \
		payments-api:latest

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Download dependencies
deps:
	go mod download

# Update dependencies
deps-update:
	go get -u ./...
	go mod tidy
