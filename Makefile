BIN     := autofan
PKG     := github.com/glfenix/autofan-for-tyanS8030
VERSION := $(shell sed -n 's/^const version = "\(.*\)"$$/\1/p' main.go)
DIST    := dist/$(BIN)-$(VERSION)

.PHONY: all build test vet fmt fmt-check cover probe clean dist deploy module release-check

all: release-check

build:
	go build -o $(BIN) .

test:
	go test ./...

cover:
	go test -cover ./...

vet:
	go vet ./...

fmt:
	gofmt -w *.go

# CI проверяет это, а не gofmt -w: формат не должен тихо править чужие диффы
fmt-check:
	@bad=$$(gofmt -l .); \
	if [ -n "$$bad" ]; then echo "не отформатировано: $$bad"; exit 1; fi

probe: build
	./$(BIN) -c examples/autofan.conf --dry-run probe

# проверочная сборка перед релизом: формат + статический анализ + тесты + сборка
release-check: fmt-check vet test build
	@echo "$(BIN) $(VERSION): ок"

# архив для установки на хост (хост может быть без тулчейна Go)
dist: build
	mkdir -p dist
	tar czf $(DIST).tgz $(BIN) install.sh examples scripts README.md LICENSE NOTICE
	@echo "готово: $(DIST).tgz"

# установка на локальный/удалённый хост: make deploy HOST=<user>@192.168.1.10
deploy: dist
	@test -n "$(HOST)" || { echo "нужен HOST=... (например HOST=<user>@192.168.1.10)"; exit 1; }
	scp -q $(DIST).tgz $(HOST):/opt/
	ssh $(HOST) 'cd /opt && tar xzf $(notdir $(DIST)).tgz && cd $(BIN)-$(VERSION) && ./install.sh'

# перед публикацией на GitHub замени путь модуля:
#   make module REPO=github.com/you/autofan
# все исходники в одном каталоге и в package main, импорта пути модуля в них
# нет - поэтому одной команды достаточно
module:
	@test -n "$(REPO)" || { echo "нужен REPO=github.com/you/autofan"; exit 1; }
	go mod edit -module $(REPO)
	gofmt -w *.go
	go mod tidy
	@echo "модуль: $(REPO)"

clean:
	rm -f $(BIN) $(BIN)-* dist/*.tgz
