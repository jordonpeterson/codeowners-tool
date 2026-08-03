.PHONY: build test docs docs-check diff-test vet all

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

# CI gate: the committed docs must be exactly what the generator produces.
# gendocs is deterministic, so a diff here means a test doc comment changed
# (or a test was added/renamed) without `make docs` being run.
docs-check: docs
	@git diff --exit-code -- docs/BEHAVIOR.md \
	  || { echo "docs/BEHAVIOR.md is stale — run 'make docs' and commit the result."; exit 1; }

# Differential fuzz of the pattern matcher against the vendored, unmodified
# hmarr/codeowners oracle (500k cases).
diff-test:
	go run ./tools/difftest 500000
