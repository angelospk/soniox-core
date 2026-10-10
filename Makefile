BIN   := bin/soniox
PLIST := com.soniox.server.plist
LA    := $(HOME)/Library/LaunchAgents/$(PLIST)

.PHONY: build test vet race install uninstall run clean

build:
	go build -o $(BIN) ./cmd/soniox

test:
	go test ./...

race:
	go test -race -shuffle=on ./...

vet:
	go vet ./...

## install: build + register the LaunchAgent (server on 127.0.0.1:8722)
install: build
	cp $(PLIST) $(HOME)/Library/LaunchAgents/
	-launchctl unload $(LA) 2>/dev/null
	launchctl load $(LA)
	@echo "soniox-core running on http://127.0.0.1:8722  (logs: /tmp/soniox-server.log)"

uninstall:
	-launchctl unload $(LA) 2>/dev/null
	rm -f $(LA)
	@echo "soniox-core uninstalled"

run: build
	$(BIN) serve

clean:
	rm -rf bin
