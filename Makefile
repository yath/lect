GOPATH = $(CURDIR)/gopath
PREFIX = $(HOME)

SRCS = main.go

lect: $(SRCS) $(shell find $(GOPATH)/src -name \*.go)
	go build -v -x -o $@ $<

.PHONY: lint
lint: $(SRCS)
	go vet $(SRCS)
	golint $(SRCS)
