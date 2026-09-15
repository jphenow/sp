.PHONY: build install test clean

# Build the sp binary for the local machine
build:
	go build -o sp-bin .

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
	rm -f sp-bin

# Tidy dependencies
tidy:
	go mod tidy

# Lint (requires golangci-lint)
lint:
	golangci-lint run ./...

# Build and install in one step
all: tidy build install
