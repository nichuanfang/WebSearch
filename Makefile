# WebSearch 构建脚本
#
# Windows 上如果没有 make，用 PowerShell 原生脚本：
#     .\build.ps1 build
#     .\build.ps1 test
#     .\build.ps1 all
#
# 有 make 时（本机装有 mingw32-make）：
#     mingw32-make build
#
# 注意：Windows 的 cmd.exe 没有 rm -rf，clean 目标在 Windows 下
# 会被 make 覆盖为 PowerShell 实现，见下方条件分支。

BINARY  := bin/websearch
PKG     := ./cmd/websearch
GOFLAGS := -trimpath

.PHONY: all build test vet fmt clean run quota config doctor build-linux

all: vet test build

build:
	go build $(GOFLAGS) -o $(BINARY) $(PKG)

test:
	go test ./... -count=1 -p 1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	./$(BINARY) serve --config config.yaml

quota: build
	./$(BINARY) quota --config config.yaml

config: build
	./$(BINARY) config --config config.yaml

doctor: build
	./$(BINARY) doctor --config config.yaml

# 交叉编译到 Linux（amd64/arm64），便于部署到服务器。
build-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o bin/websearch-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o bin/websearch-linux-arm64 $(PKG)

# Windows 下用 PowerShell 删除目录（cmd.exe 没有 rm -rf）。
ifeq ($(OS),Windows_NT)
clean:
	powershell -NoProfile -Command "Remove-Item -Recurse -Force bin,data -ErrorAction SilentlyContinue"
else
clean:
	rm -rf bin data
endif
