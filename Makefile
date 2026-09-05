.PHONY: all build test cover clean

all: build

build:
	go build -trimpath -o tagbrr .

test:
	go test -shuffle=on -race ./...

cover:
	go test -cover -coverprofile=coverage.out .
	@go tool cover -func=coverage.out | tail -1

clean:
	rm -f tagbrr coverage.out
