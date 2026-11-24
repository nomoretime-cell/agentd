.PHONY: bootstrap build test race vet run live dingtalk-test dingtalk-live
bootstrap:
	./scripts/bootstrap
build:
	CGO_ENABLED=0 ./scripts/go build -trimpath -o bin/agentd ./cmd/agentd
test:
	./scripts/go test ./...
race:
	CGO_ENABLED=1 ./scripts/go test -race ./...
vet:
	./scripts/go vet ./...
run: build
	./bin/agentd -config config.yaml
live:
	AGENTD_LIVE=1 ./scripts/go test -v ./internal/agent -run TestLive -count=1 -timeout 10m

dingtalk-test:
	./scripts/go test -v ./internal/agent -run TestDingTalk -count=1
dingtalk-live:
	AGENTD_LIVE=1 ./scripts/go test -v ./internal/agent -run '^TestDingTalkLiveLocalModel$$' -count=1 -timeout 4m

.PHONY: release
PNPM ?= pnpm
release:
	$(PNPM) --dir web install --frozen-lockfile
	$(PNPM) --dir web run build
	rm -rf cmd/agentd/ui
	cp -R web/dist cmd/agentd/ui
	CGO_ENABLED=0 ./scripts/go build -tags webui -trimpath -o bin/agentd-web ./cmd/agentd
