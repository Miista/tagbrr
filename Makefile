.PHONY: all build test test-unit test-integration cover clean

all: build

build:
	go build -trimpath -o tagbrr .

test: test-unit test-integration

test-unit:
	go test -shuffle=on -race .

# Needs docker, contacts nothing upstream: the subject builds from the
# shipped Dockerfile, the mock from its own source into scratch. -count=1
# because the tests depend on a daemon and containers the go cache cannot
# see; -p 1 because scenarios share container names and the testbed.
test-integration:
	go test -tags integration -count=1 -p 1 -shuffle=on ./test/...

cover:
	go test -cover -coverprofile=coverage.out .
	@go tool cover -func=coverage.out | tail -1

clean:
	rm -f tagbrr coverage.out
