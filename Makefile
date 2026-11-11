APP := watchfilms
LDFLAGS := -s -w

.PHONY: all linux windows yandex test clean

all: yandex linux windows

yandex:
	go run scripts/build_yandex.go

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .
	mkdir -p dist && cp $(APP) dist/$(APP)

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(APP).exe .
	mkdir -p dist && cp $(APP).exe dist/$(APP).exe

test:
	go vet ./... && go test ./...

clean:
	rm -rf dist $(APP) $(APP).exe
