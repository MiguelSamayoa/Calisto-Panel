# Comodidad opcional: `make` no viene con Windows. La fuente de verdad son los
# comandos de CLAUDE.md; este archivo solo los envuelve.

.PHONY: build run test lint fmt

build:
	go build -o C:/Tools/calisto-panel/panel.exe ./cmd/panel

run:
	go run ./cmd/panel -config ./config.yaml

test:
	go test ./...

lint:
	go vet ./...
	golangci-lint run

fmt:
	gofmt -l .
