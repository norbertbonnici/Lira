BINARY := lira

# Host to deploy to, e.g. make deploy LIRA_HOST=user@your-server
LIRA_HOST ?=

.PHONY: run build linux tidy clean deploy

# Local dev run
run:
	go run .

# Static binary for the host you build on
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BINARY) .

# Cross-compile a static binary for the ZimaBoard (x86-64 Linux)
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(BINARY)-linux-amd64 .

# Build the Linux binary and deploy it to LIRA_HOST (stop -> swap -> start,
# with backup + auto-rollback handled by deploy/deploy.sh on the host).
deploy: linux
	@test -n "$(LIRA_HOST)" || { echo "Set LIRA_HOST, e.g. make deploy LIRA_HOST=user@your-server"; exit 1; }
	scp $(BINARY)-linux-amd64 deploy/deploy.sh $(LIRA_HOST):/tmp/
	ssh -t $(LIRA_HOST) 'sudo /tmp/deploy.sh /tmp/$(BINARY)-linux-amd64 && rm -f /tmp/deploy.sh /tmp/$(BINARY)-linux-amd64'

# Resolve dependencies (run this first, once)
tidy:
	go mod tidy

clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64
