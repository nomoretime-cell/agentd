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
