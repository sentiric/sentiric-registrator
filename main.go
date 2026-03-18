// Dosya: main.go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	"github.com/rs/zerolog"
)

// Config - Uygulama konfigürasyonu
type Config struct {
	NodeIP         string
	ConsulURL      string
	HostName       string
	Env            string
	LogLevel       string
	LogFormat      string
	ServiceVersion string
}

// setupLogger: SUTS v4.0 standartlarına uygun loglayıcı üretir
func setupLogger(cfg Config) zerolog.Logger {
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}

	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFieldName = "ts"
	zerolog.LevelFieldName = "severity"
	zerolog.MessageFieldName = "message"
	zerolog.LevelFieldMarshalFunc = func(l zerolog.Level) string {
		return strings.ToUpper(l.String())
	}

	resourceContext := zerolog.Dict().
		Str("service.name", "registrator-service").
		Str("service.version", cfg.ServiceVersion).
		Str("service.env", cfg.Env).
		Str("host.name", cfg.HostName)

	var zlogger zerolog.Logger
	if cfg.LogFormat == "json" {
		zlogger = zerolog.New(os.Stdout).With().
			Timestamp().
			Str("schema_v", "1.0.0").
			Str("tenant_id", "system").
			Dict("resource", resourceContext).
			Logger()
	} else {
		output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
		zlogger = zerolog.New(output).With().Timestamp().Str("service", "registrator").Logger()
	}

	return zlogger.Level(level)
}

func main() {
	// 1. Konfigürasyon Yükle
	cfg := loadConfig()
	logger := setupLogger(cfg)

	logger.Info().
		Str("event", "SYSTEM_STARTUP").
		Str("node_ip", cfg.NodeIP).
		Str("consul_url", cfg.ConsulURL).
		Msg("🚀 Registrator başlatılıyor...")

	// 2. Docker Client Başlat
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Docker Client Hatası")
	}

	// 3. Consul Client Başlat
	consulConfig := consul.DefaultConfig()
	consulConfig.Address = strings.Replace(cfg.ConsulURL, "http://", "", 1)
	consulClient, err := consul.NewClient(consulConfig)
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Consul Client Hatası")
	}

	// 4. Bağlantı Testi
	_, err = consulClient.Agent().NodeName()
	if err != nil {
		logger.Warn().Err(err).Msg("⚠️ Consul'a erişilemiyor. Retry bekleniyor...")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 5. Mevcut Containerları Tara (Reconciliation)
	logger.Info().Str("event", "CONTAINER_SCAN_START").Msg("🔍 Mevcut containerlar taranıyor...")
	containers, err := dockerCli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		logger.Error().Err(err).Msg("❌ Container listesi alınamadı")
	} else {
		for _, c := range containers {
			processContainer(ctx, dockerCli, consulClient, c.ID, "register", cfg, logger)
		}
	}

	// 6. Event Loop Başlat
	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")
	eventFilters.Add("event", "start")
	eventFilters.Add("event", "die") // Stop/Kill durumunda silmek için

	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: eventFilters})

	// Graceful Shutdown Channel
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info().Str("event", "EVENT_LOOP_START").Msg("👂 Docker Event Loop Dinleniyor...")

	for {
		select {
		case err := <-errs:
			logger.Fatal().Err(err).Msg("🔥 Kritik Docker Event Hatası")

		case msg := <-msgs:
			go func(m events.Message) {
				if m.Action == "start" {
					time.Sleep(1 * time.Second)
					processContainer(ctx, dockerCli, consulClient, m.ID, "register", cfg, logger)
				} else if m.Action == "die" {
					deregisterContainer(consulClient, m.ID, logger)
				}
			}(msg)

		case <-sigChan:
			logger.Info().Str("event", "SYSTEM_SHUTDOWN").Msg("🛑 Kapatılıyor...")
			return
		}
	}
}

// processContainer: Tek bir containerı analiz eder ve Consul'a kaydeder
func processContainer(ctx context.Context, d *client.Client, c *consul.Client, containerID string, action string, cfg Config, logger zerolog.Logger) {
	cj, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
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

	ignoreRanges := parseIgnorePorts(envMap["SERVICE_IGNORE_PORTS"])

	// Servis İsmi Belirle
	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-")
	}
	serviceName = sanitizeName(serviceName)

	ports := cj.NetworkSettings.Ports
	if len(ports) == 0 {
		return
	}

	for portKey, bindings := range ports {
		if len(bindings) > 0 {
			proto := portKey.Proto()
			hostPortStr := bindings[0].HostPort
			hostPort, _ := strconv.Atoi(hostPortStr)

			if isPortIgnored(hostPort, ignoreRanges) {
				continue
			}

			serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)

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
				Check: &consul.AgentServiceCheck{
					Name:                           fmt.Sprintf("TCP Check %s:%d", serviceName, hostPort),
					TCP:                            fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
					Interval:                       "15s",
					Timeout:                        "5s",
					DeregisterCriticalServiceAfter: "1m",
				},
			}

			err := c.Agent().ServiceRegister(registration)
			if err != nil {
				logger.Error().
					Str("event", "SERVICE_REGISTER_FAILED").
					Err(err).
					Str("registered_service", serviceName).
					Msg("❌ Kayıt Başarısız")
			} else {
				logger.Info().
					Str("event", "SERVICE_REGISTERED").
					Str("registered_service", serviceName).
					Str("target_ip", cfg.NodeIP).
					Int("target_port", hostPort).
					Msgf("➕ KAYIT EDİLDİ: %s", serviceName)
			}
		}
	}
}

// deregisterContainer: Kapanan containerın servislerini siler
func deregisterContainer(c *consul.Client, containerID string, logger zerolog.Logger) {
	// Consul TTL/Check mantığıyla kendi siliyor. Şimdilik pasif bırakıyoruz.
}

type PortRange struct {
	Start int
	End   int
}

func parseIgnorePorts(envVal string) []PortRange {
	var ranges []PortRange
	if envVal == "" {
		return ranges
	}

	parts := strings.Split(envVal, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.Contains(p, "-") {
			bounds := strings.Split(p, "-")
			if len(bounds) == 2 {
				start, _ := strconv.Atoi(strings.TrimSpace(bounds[0]))
				end, _ := strconv.Atoi(strings.TrimSpace(bounds[1]))
				if start > 0 && end > 0 {
					ranges = append(ranges, PortRange{Start: start, End: end})
				}
			}
		} else {
			port, _ := strconv.Atoi(p)
			if port > 0 {
				ranges = append(ranges, PortRange{Start: port, End: port})
			}
		}
	}
	return ranges
}

// isPortIgnored: Bir portun yasaklı listede olup olmadığını kontrol eder
func isPortIgnored(port int, ranges []PortRange) bool {
	for _, r := range ranges {
		if port >= r.Start && port <= r.End {
			return true
		}
	}
	return false
}

// [ARCH-COMPLIANCE] constraints.yaml gereğince üretim (production) loglarının SUTS v4.0 JSON formatında
// basılabilmesi için config yapısı env okumalarını destekleyecek şekilde güncellendi.
func loadConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		NodeIP:         getEnv("NODE_IP", "127.0.0.1"),
		ConsulURL:      getEnv("CONSUL_URL", "http://discovery-service:8500"),
		HostName:       getEnv("NODE_HOSTNAME", hostname),
		Env:            getEnv("APP_ENV", "production"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		LogFormat:      getEnv("LOG_FORMAT", "json"), // Default olarak JSON dayatılır
		ServiceVersion: getEnv("SERVICE_VERSION", "1.0.0"),
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
