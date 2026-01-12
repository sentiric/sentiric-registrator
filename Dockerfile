# ==========================================
# STAGE 1: BUILDER (Golang)
# ==========================================
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Dependency Cache
COPY go.mod go.sum ./
RUN go mod download

# Source Code
COPY . .

# Build Static Binary
# CGO_ENABLED=0 -> System library bağımlılığını kaldırır
# -ldflags="-s -w" -> Debug bilgilerini siler (Dosya boyutunu küçültür)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o registrator .

# ==========================================
# STAGE 2: RUNNER (Alpine - Minimal)
# ==========================================
FROM alpine:latest

# SSL sertifikaları ve temel araçlar
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

# Binary'i kopyala
COPY --from=builder /app/registrator .

# Çalıştırma
CMD ["./registrator"]