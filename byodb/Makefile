.PHONY: test test-portable check cross race vet fmt cover bench cli fuzz all

all: fmt vet test

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test -count=1 ./...

# The page backend Windows uses, exercised here. Without this, that code path
# would only ever run on Windows — which is where it is hardest to test.
test-portable:
	go test -count=1 -tags byodb_pread ./...

# Everything a change should pass before it is trusted.
check: vet test test-portable
	go test -count=1 -race ./...
	GOOS=windows go vet ./...
	GOOS=darwin go vet ./...

cross:
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=386 go build ./...
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=linux GOARCH=arm64 go build ./...

race:
	go test -count=1 -race ./...

cover:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

bench:
	go test -run '^$$' -bench . -benchmem ./...

fuzz:
	go test -run '^$$' -fuzz FuzzParse -fuzztime 60s ./ql/

cli:
	go build -o byodb ./cmd/byodb
