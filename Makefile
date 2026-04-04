# Değişkenler
GOPATH=$(shell go env GOPATH)
LINT_BIN=$(GOPATH)/bin/golangci-lint
# Projedeki 'package main' içeren dizini otomatik bulur (cmd/app, root, vb.)
MAIN_DIR=$(shell find . -name "*.go" -not -path "./vendor/*" -exec grep -l "package main" {} + | xargs -n1 dirname | sort -u | head -n 1)
BINARY_NAME=$(shell basename $(CURDIR))

.PHONY: all setup fmt lint build test clean run

# Varsayılan akış
all: setup fmt lint test build

setup:
	@echo "🛠️ Araçlar kontrol ediliyor ve yükleniyor..."
	@# Go 1.24+ uyumlu linter kurulumu
	@if [ ! -f $(LINT_BIN) ]; then \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(GOPATH)/bin v1.64.5; \
	fi
	go install mvdan.cc/gofumpt@latest

fmt:
	@echo "🧹 Kod formatlanıyor (gofumpt)..."
	go fmt ./...
	@# gofumpt yüklendiyse çalıştır
	@$(GOPATH)/bin/gofumpt -l -w .

lint:
	@echo "🔍 Linter çalışıyor (golangci-lint)..."
	@# Hata olsa bile devam etmesini istersen başına '-' koyabilirsin: -$(LINT_BIN)...
	$(LINT_BIN) run ./... --fix

build:
	@echo "🏗️ Binary inşa ediliyor..."
	@mkdir -p bin
	@if [ -z "$(MAIN_DIR)" ]; then \
		echo "❌ Hata: 'package main' içeren bir Go dosyası bulunamadı!"; \
		exit 1; \
	fi
	@echo "📍 Kaynak: $(MAIN_DIR) -> Hedef: bin/$(BINARY_NAME)"
	go build -o bin/$(BINARY_NAME) $(MAIN_DIR)/.

test:
	@echo "🧪 Testler koşturuluyor (Race Detector açık)..."
	go test -v -race ./...

clean:
	@echo "🗑️ Temizleniyor..."
	go clean
	rm -rf bin/

run:
	@echo "🚀 Uygulama başlatılıyor..."
	@if [ -z "$(MAIN_DIR)" ]; then \
		echo "❌ Hata: Çalıştırılacak 'main' paketi bulunamadı!"; \
		exit 1; \
	fi
	go run $(MAIN_DIR)/.