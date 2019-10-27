SRCS = main.go bulb.go serial.go

lect: $(SRCS) go.mod
	go build -v -x -o $@ $(SRCS)

.PHONY: lint
lint: $(SRCS)
	go vet $(SRCS)
	golint $(SRCS)
