.PHONY: build test docs diff-test vet all

all: vet test build docs

build:
	go build -o bin/codeowners-tool ./cmd/codeowners-tool

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l . | (! grep .) || (echo "gofmt needed" && exit 1)

# Regenerate docs/BEHAVIOR.md from test doc comments (tests ARE the docs).
docs:
	go run ./tools/gendocs

# Differential fuzz of the pattern matcher against the vendored, unmodified
# hmarr/codeowners oracle (500k cases).
diff-test:
	go run ./tools/difftest 500000
