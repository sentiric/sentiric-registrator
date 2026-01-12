# Sentiric Registrator

**Sentiric Registrator**, Docker container eventlerini dinleyen ve çalışan servisleri otomatik olarak **Consul Discovery Service**'e kaydeden hafif, Go tabanlı bir araçtır.

## 🚀 Özellikler

- **Otomatik Keşif:** `docker run` dediğiniz anda servis Consul'a kaydedilir.
- **Sıfır Bağımlılık:** Her container'a agent kurmanıza gerek yok. Host başına 1 adet çalışır.
- **Akıllı Filtreleme:** `SERVICE_IGNORE=true` ile istenmeyen servisleri kaydetmez.
- **Port Mapping:** Docker'ın rastgele atadığı portları (HostPort) doğru şekilde Consul'a bildirir.

## 🛠️ Kurulum (Docker Compose)

Sentiric Infrastructure içinde aşağıdaki gibi kullanılır:

```yaml
services:
  registrator-service:
    image: ghcr.io/sentiric/sentiric-registrator:latest
    container_name: sentiric-registrator
    restart: always
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock # ZORUNLU
    environment:
      - NODE_IP=${NODE_IP} # Host makinenin IP'si
      - CONSUL_URL=http://discovery-service:8500
      - SERVICE_IGNORE=true
    depends_on:
      discovery-service:
        condition: service_healthy
```

## ⚙️ Environment Variables (Container İçi)

Servislerinizin içinde aşağıdaki değişkenleri kullanarak Registrator'ı yönlendirebilirsiniz:

| Değişken | Açıklama | Örnek |
| :--- | :--- | :--- |
| `SERVICE_NAME` | Servis ismini manuel belirler. | `auth-service` |
| `SERVICE_IGNORE` | `true` ise bu servisi kaydetmez. | `true` |

## 🏗️ Geliştirme

```bash
# Bağımlılıkları yükle
go mod tidy

# Derle
go build -o registrator .

# Çalıştır (Local Docker Socket gerekli)
NODE_IP=127.0.0.1 CONSUL_URL=http://localhost:8500 ./registrator
```


---
