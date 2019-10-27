SRCS = main.go bulb.go serial.go

.PHONY: all
all: lect test

lect: $(SRCS) go.mod
	go build -v -x -o $@ $(SRCS)

.PHONY: test
test:
	go test

.PHONY: lint
lint: $(SRCS)
	go vet $(SRCS)
	golint $(SRCS)
