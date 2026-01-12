package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container" // YENİ: ContainerListOptions burada
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
)

// Config - Uygulama konfigürasyonu
type Config struct {
	NodeIP    string
	ConsulURL string
	HostName  string
}

// Global Logger
var logger = log.New(os.Stdout, "[SENTIRIC-REGISTRATOR] ", log.LstdFlags)

func main() {
	// 1. Konfigürasyon Yükle
	cfg := loadConfig()
	logger.Printf("🚀 Başlatılıyor... Node: %s | Consul: %s", cfg.NodeIP, cfg.ConsulURL)

	// 2. Docker Client Başlat
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatalf("❌ Docker Client Hatası: %v", err)
	}

	// 3. Consul Client Başlat
	consulConfig := consul.DefaultConfig()
	consulConfig.Address = strings.Replace(cfg.ConsulURL, "http://", "", 1)
	consulClient, err := consul.NewClient(consulConfig)
	if err != nil {
		logger.Fatalf("❌ Consul Client Hatası: %v", err)
	}

	// 4. Bağlantı Testi
	_, err = consulClient.Agent().NodeName()
	if err != nil {
		logger.Printf("⚠️ UYARI: Consul'a erişilemiyor (%v). Retry bekleniyor...", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 5. Mevcut Containerları Tara (Reconciliation)
	logger.Println("🔍 Mevcut containerlar taranıyor...")
	// DÜZELTME: types.ContainerListOptions -> container.ListOptions
	containers, err := dockerCli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		logger.Printf("❌ Container listesi alınamadı: %v", err)
	} else {
		for _, c := range containers {
			processContainer(ctx, dockerCli, consulClient, c.ID, "register", cfg)
		}
	}

	// 6. Event Loop Başlat
	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")
	eventFilters.Add("event", "start")
	eventFilters.Add("event", "die") // Stop/Kill durumunda silmek için

	// DÜZELTME: types.EventsOptions (Hala types altında ama kontrol edilmeli)
	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: eventFilters})

	// Graceful Shutdown Channel
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Println("👂 Docker Event Loop Dinleniyor...")

	for {
		select {
		case err := <-errs:
			logger.Fatalf("🔥 Kritik Docker Event Hatası: %v", err)

		case msg := <-msgs:
			// Event İşleyici
			go func(m events.Message) {
				if m.Action == "start" {
					// Ağın oturması için kısa bekleme
					time.Sleep(1 * time.Second)
					processContainer(ctx, dockerCli, consulClient, m.ID, "register", cfg)
				} else if m.Action == "die" {
					// Container durduğunda deregister işlemi
					deregisterContainer(consulClient, m.ID)
				}
			}(msg)

		case <-sigChan:
			logger.Println("🛑 Kapatılıyor...")
			return
		}
	}
}

// processContainer: Tek bir containerı analiz eder ve Consul'a kaydeder
func processContainer(ctx context.Context, d *client.Client, c *consul.Client, containerID string, action string, cfg Config) {
	// Container Detaylarını Çek
	cj, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
		// Container o an silinmiş olabilir
		return
	}

	// Environment Variable Parse
	envMap := make(map[string]string)
	for _, e := range cj.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = cleanString(parts[1])
		}
	}

	// IGNORE Kontrolü
	if envMap["SERVICE_IGNORE"] == "true" {
		return
	}

	// Servis İsmi Belirle
	// Öncelik: ENV > Container Name
	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-") // Prefix temizliği
	}
	serviceName = sanitizeName(serviceName)

	// Port Mapping Kontrolü
	ports := cj.NetworkSettings.Ports
	if len(ports) == 0 {
		return
	}

	for portKey, bindings := range ports {
		if len(bindings) > 0 {
			// portKey örneğin: "80/tcp"
			proto := portKey.Proto()
			hostPortStr := bindings[0].HostPort
			hostPort, _ := strconv.Atoi(hostPortStr)

			// ID Unique olmalı: Hostname-ServiceName-Port
			serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)

			// Consul Payload
			registration := &consul.AgentServiceRegistration{
				ID:      serviceID,
				Name:    serviceName,
				Port:    hostPort,
				Address: cfg.NodeIP,
				Tags:    []string{proto, "sentiric", "auto-registered"},
				Meta: map[string]string{
					"container_id": cj.ID[:12],
					"image":        cj.Config.Image,
				},
				// TCP Health Check
				Check: &consul.AgentServiceCheck{
					Name:                           fmt.Sprintf("TCP Check %s", serviceName),
					TCP:                            fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
					Interval:                       "15s",
					Timeout:                        "5s",
					DeregisterCriticalServiceAfter: "1m",
				},
			}

			// Kayıt İşlemi
			err := c.Agent().ServiceRegister(registration)
			if err != nil {
				logger.Printf("❌ Kayıt Başarısız: %s -> %v", serviceName, err)
			} else {
				logger.Printf("➕ KAYIT EDİLDİ: %s [%s] -> %s:%d", serviceName, serviceID, cfg.NodeIP, hostPort)
			}
		}
	}
}

// deregisterContainer: Kapanan containerın servislerini siler
func deregisterContainer(c *consul.Client, containerID string) {
	// Not: Opsiyonel logic
}

// Yardımcı Fonksiyonlar
func loadConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		NodeIP:    getEnv("NODE_IP", "127.0.0.1"),
		ConsulURL: getEnv("CONSUL_URL", "http://discovery-service:8500"),
		HostName:  getEnv("NODE_HOSTNAME", hostname),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func cleanString(s string) string {
	return strings.Trim(s, "\"' ")
}

func sanitizeName(s string) string {
	return strings.ToLower(s)
}