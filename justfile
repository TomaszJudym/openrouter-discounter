# Pin to the go.mod version: go1.27.3 export data (v5) is not yet
# parseable by golangci-lint 2.13.2, but go1.27.1 (v4) is.
export GOTOOLCHAIN := "go1.27.1"

alias t := test
alias l := lint
alias c := check

# All checks: build, vet, race-test, lint.
check: test lint

# Build, vet, and race-test every package.
test:
	go build ./...
	go vet ./...
	go test -race -count=1 ./...

# Static checks and formatting (golangci-lint v2, config in .golangci.yml).
lint:
	golangci-lint run ./...

# Format all Go files in place.
fmt:
	golangci-lint fmt ./...

# Fix lint/format issues in place.
fix:
	golangci-lint run --fix ./...
