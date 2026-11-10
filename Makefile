.PHONY: build run test demo clean

build:
	go build -o bin/ ./cmd/...

run: build
	./bin/amp serve --embedded-db

test:
	go test -count=1 ./...

demo: build
	./scripts/demo.sh

clean:
	rm -rf bin
