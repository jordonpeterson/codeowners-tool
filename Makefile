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
# hmarr/codeowners oracle (500k cases). This is exactly what CI runs on every PR
# — same count, same default seed — so a green run here is a green gate there.
# Pass a seed to explore elsewhere: go run ./tools/difftest 500000 42
diff-test:
	go run ./tools/difftest 500000
