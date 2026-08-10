package messaging

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const providerRateLimitStateTTL = 35 * 24 * time.Hour
const providerRateLimitMaxValueSize = 64 * 1024

// NATSClient wraps the nats.Conn
type NATSClient struct {
	Conn   *nats.Conn
	JS     nats.JetStreamContext
	server *server.Server
}

func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".fused-nats-write-check-*")
	if err != nil {
		return err
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return nil
}

func makeAbs(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func resolveEmbeddedNATSStoreDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("FUSED_NATS_STORE_DIR")); override != "" {
		dir := makeAbs(override)
		if err := ensureWritableDir(dir); err != nil {
			return "", fmt.Errorf("FUSED_NATS_STORE_DIR %q is not writable: %w", dir, err)
		}
		return dir, nil
	}

	var candidates []string
	if exePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exePath), "data", "nats"))
	}
	if wd, err := os.Getwd(); err == nil {
		fallback := filepath.Join(wd, "data", "nats")
		if len(candidates) == 0 || candidates[0] != fallback {
			candidates = append(candidates, fallback)
		}
	}

	var lastErr error
	for _, dir := range candidates {
		if err := ensureWritableDir(dir); err != nil {
			lastErr = err
			slog.Warn("Embedded NATS store directory unavailable", slog.String("path", dir), slog.Any("error", err))
			continue
		}
		return dir, nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("failed to find writable embedded NATS store directory: %w", lastErr)
	}
	return "", fmt.Errorf("failed to determine embedded NATS store directory")
}

// ConnectNATS establishes a connection to the NATS server.
func ConnectNATS() (*NATSClient, error) {
	config, err := loadNATSConnectionConfig()
	if err != nil {
		return nil, err
	}
	var embeddedServer *server.Server
	if !config.external {
		embeddedServer, err = ensureEmbeddedNATS(config.url)
		if err != nil {
			return nil, err
		}
	}

	opts := []nats.Option{
		nats.Name("fused-engine"),
		nats.MaxReconnects(5),
		nats.ReconnectWait(2 * time.Second),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				slog.Warn("Disconnected from NATS", slog.Any("error", err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("Reconnected to NATS", slog.String("mode", natsConnectionMode(config)))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			slog.Info("NATS connection closed")
		}),
	}
	opts = append(opts, config.clientOptions...)

	nc, err := nats.Connect(config.url, opts...)
	if err != nil {
		if embeddedServer != nil {
			embeddedServer.Shutdown()
		}
		return nil, err
	}

	slog.Info("Connected to NATS",
		slog.String("mode", natsConnectionMode(config)),
		slog.String("authentication", config.authMethod),
		slog.Bool("tls", config.tlsEnabled),
	)

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		if embeddedServer != nil {
			embeddedServer.Shutdown()
		}
		return nil, err
	}

	return &NATSClient{Conn: nc, JS: js, server: embeddedServer}, nil
}

func ensureEmbeddedNATS(natsURL string) (*server.Server, error) {
	// The probe only elects a convenience follower on the same host; embedded
	// mode is deliberately not presented as a resilient Engine cluster.
	ncCheck, err := nats.Connect(natsURL, nats.NoReconnect(), nats.Timeout(time.Second))
	if err == nil {
		ncCheck.Close()
		slog.Info("Found existing local NATS server, skipping embedded boot", slog.String("mode", "local_follower"))
		return nil, nil
	}
	storeDir, err := resolveEmbeddedNATSStoreDir()
	if err != nil {
		return nil, fmt.Errorf("resolve embedded NATS store directory: %w", err)
	}
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: 4222, JetStream: true, StoreDir: storeDir})
	if err != nil {
		return nil, fmt.Errorf("initialize embedded NATS server: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("embedded NATS server failed to start within 5s")
	}
	slog.Info("Embedded NATS server is ready", slog.String("mode", "embedded"), slog.String("store_dir", storeDir))
	return ns, nil
}

func natsConnectionMode(config natsConnectionConfig) string {
	if config.external {
		return "external"
	}
	return "embedded"
}

// InitStream creates or updates a JetStream stream
func (c *NATSClient) InitStream(streamName string, subjects []string) error {
	info, err := c.JS.StreamInfo(streamName)
	if err != nil {
		_, err = c.JS.AddStream(&nats.StreamConfig{
			Name:     streamName,
			Subjects: subjects,
			MaxAge:   30 * 24 * time.Hour, // 30 days retention
		})
		if err != nil {
			return err
		}
		slog.Info("Created JetStream stream", slog.String("stream", streamName))
	} else {
		// Update stream subjects in case they have changed
		config := info.Config

		// Merge existing subjects with new subjects
		subjectMap := make(map[string]bool)
		for _, s := range config.Subjects {
			subjectMap[s] = true
		}
		for _, s := range subjects {
			subjectMap[s] = true
		}

		var mergedSubjects []string
		for s := range subjectMap {
			mergedSubjects = append(mergedSubjects, s)
		}

		config.Subjects = mergedSubjects
		_, err = c.JS.UpdateStream(&config)
		if err != nil {
			return err
		}
	}
	return nil
}

// InitProviderRateLimitBucket provisions the compacted, replicated state used
// on the provider execution path. A bounded TTL prevents abandoned service
// versions from accumulating forever; PostgreSQL can conservatively recover a
// scope if it is used again after expiry.
func (c *NATSClient) InitProviderRateLimitBucket() (nats.KeyValue, error) {
	if c == nil || c.JS == nil {
		return nil, errors.New("JetStream client is required")
	}
	if existing, err := c.JS.KeyValue(ProviderRateLimitBucket); err == nil {
		return existing, c.reconcileProviderRateLimitBucket()
	}
	config := &nats.KeyValueConfig{
		Bucket: ProviderRateLimitBucket, Description: "Fused provider rate-limit coordination state",
		History: 1, TTL: providerRateLimitStateTTL, MaxValueSize: providerRateLimitMaxValueSize, Storage: nats.FileStorage,
		Replicas: providerRateLimitReplicas(), Compression: true,
	}
	created, createErr := c.JS.CreateKeyValue(config)
	if createErr == nil {
		return created, nil
	}
	// Another Engine may have won the create race. Loading the now-existing
	// bucket distinguishes that benign case from a real provisioning failure.
	existing, loadErr := c.JS.KeyValue(ProviderRateLimitBucket)
	if loadErr == nil {
		return existing, nil
	}
	return nil, fmt.Errorf("create provider rate-limit JetStream KV: %w", createErr)
}

func (c *NATSClient) reconcileProviderRateLimitBucket() error {
	info, err := c.JS.StreamInfo(ProviderRateLimitKVStream())
	if err != nil {
		return fmt.Errorf("inspect provider rate-limit JetStream KV: %w", err)
	}
	desiredReplicas := providerRateLimitReplicas()
	if info.Config.Replicas == desiredReplicas && info.Config.MaxAge == providerRateLimitStateTTL && info.Config.MaxMsgSize == providerRateLimitMaxValueSize {
		return nil
	}
	// Preserve the KV stream's subjects and compaction flags while converging
	// only the durability and bounded-retention settings owned by Engine.
	config := info.Config
	config.Replicas = desiredReplicas
	config.MaxAge = providerRateLimitStateTTL
	config.MaxMsgSize = providerRateLimitMaxValueSize
	if _, err := c.JS.UpdateStream(&config); err != nil {
		return fmt.Errorf("update provider rate-limit JetStream KV: %w", err)
	}
	return nil
}

func providerRateLimitReplicas() int {
	raw := strings.TrimSpace(os.Getenv("FUSED_NATS_RATE_LIMIT_REPLICAS"))
	if raw == "" {
		return 1
	}
	replicas, err := strconv.Atoi(raw)
	if err != nil || replicas < 1 || replicas > 5 {
		slog.Warn("Invalid FUSED_NATS_RATE_LIMIT_REPLICAS; using one replica")
		return 1
	}
	return replicas
}

// Publish is a simple wrapper to publish a message
func (c *NATSClient) Publish(subject string, data []byte) error {
	return c.Conn.Publish(subject, data)
}

// PublishJS publishes a message to JetStream
func (c *NATSClient) PublishJS(subject string, data []byte) (*nats.PubAck, error) {
	return c.JS.Publish(subject, data)
}

// PublishMsgJS publishes a message with headers to JetStream
func (c *NATSClient) PublishMsgJS(msg *nats.Msg) (*nats.PubAck, error) {
	return c.JS.PublishMsg(msg)
}

// Subscribe is a simple wrapper to subscribe to a subject
func (c *NATSClient) Subscribe(subject string, cb nats.MsgHandler) (*nats.Subscription, error) {
	return c.Conn.Subscribe(subject, cb)
}

// Close closes the NATS connection
func (c *NATSClient) Close() {
	if c.Conn != nil {
		c.Conn.Close()
	}
	if c.server != nil {
		c.server.Shutdown()
	}
}
