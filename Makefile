.PHONY: build run dev clean docker

build:
	go build -o jumia-server ./cmd/

run: build
	./jumia-server

dev:
	go run ./cmd/

clean:
	rm -f jumia-server jumia.json

docker:
	docker build -t jumia-mvp .
	docker run -p 8080:8080 jumia-mvp

test:
	go build ./...
	@echo "✅ Build OK"
