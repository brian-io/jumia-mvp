.PHONY: build run dev clean docker

build:
	go build -o agora-server ./cmd/

run: build
	./agora-server

dev:
	go run ./cmd/

clean:
	rm -f agora-server agora.json

docker:
	docker build -t agora .
	docker run -p 8080:8080 agora

test:
	go build ./...
	@echo "✅ Build OK"
