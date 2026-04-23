.PHONY: build build-sprite install test clean proto

# Build the sp binary for the local machine
build:
	go build -o sp-bin .

# Build the sp binary for sprite environments (linux/amd64)
build-sprite:
	GOOS=linux GOARCH=amd64 go build -o sp-linux-amd64 .

# Install sp to GOPATH/bin
install:
	go install .

# Run all tests
test:
	go test ./... -v

# Run tests with race detector
test-race:
	go test ./... -race -v

# Clean build artifacts
clean:
	rm -f sp-bin sp-linux-amd64

# Tidy dependencies
tidy:
	go mod tidy

# Generate protobuf (for future gRPC integration)
proto:
	@echo "Proto generation not yet configured"

# Lint (requires golangci-lint)
lint:
	golangci-lint run ./...

# Build and install in one step
all: tidy build install
