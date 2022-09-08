image := docker.io/linjinbao66/indeed-csi
version := 1.0.0
tag := latest
CGO_ENABLED := 0
GOOS := linux
GOARCH := amd64

.PHONY: build image image2 clean lint help

all: build

build:
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -a -ldflags ' -X main.version=$(version) -extldflags "-static"' -o indeed-csi .

image:
	docker build -t $(image):$(tag) -f Dockerfile .

image2:
	docker build -t $(image):1.0.0 -f Dockerfile2 .

lint:


clean:
	rm -rf indeed-csi
	go clean -i .

help:
	@echo "make: compile packages and dependencies"
	@echo "make image: build image"
	@echo "make image2: build image from local"
	@echo "make lint: golint ./..."
	@echo "make clean: remove object files and cached files"