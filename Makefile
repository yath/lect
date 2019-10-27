SRCS = main.go

lect: $(SRCS) go.mod
	go build -v -x -o $@ $<

.PHONY: lint
lint: $(SRCS)
	go vet $(SRCS)
	golint $(SRCS)
