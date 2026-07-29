package messaging

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// NATSClient wraps the nats.Conn
type NATSClient struct {
	Conn *nats.Conn
	JS   nats.JetStreamContext
}

// ConnectNATS establishes a connection to the NATS server.
func ConnectNATS() (*NATSClient, error) {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL

		// Smart Leader Logic: Check if NATS is already running locally
		// We use a short timeout and disable reconnects so we fail fast if nothing is listening.
		ncCheck, err := nats.Connect(natsURL, nats.NoReconnect(), nats.Timeout(1*time.Second))
		if err == nil {
			// A NATS server is already running! We act as a Follower.
			ncCheck.Close()
			slog.Info("Found existing local NATS server, skipping embedded boot (Follower Mode)")
		} else {
			// No NATS server is running. We act as the Leader and boot the embedded server.
			slog.Info("No local NATS server found. Booting embedded NATS server on port 4222... (Leader Mode)")

			// We need a robust data directory for JetStream to persist stream data locally.
			opts := &server.Options{
				Port:      4222,
				JetStream: true,
				StoreDir:  "data/nats",
			}

			ns, err := server.NewServer(opts)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize embedded NATS server: %w", err)
			}

			go ns.Start()

			if !ns.ReadyForConnections(5 * time.Second) {
				return nil, fmt.Errorf("embedded NATS server failed to start within 5s")
			}
			slog.Info("Embedded NATS server is ready for connections")
		}
	}

	opts := []nats.Option{
		nats.Name("fused-api"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(5),
		nats.ReconnectWait(2 * time.Second),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				slog.Warn("Disconnected from NATS", slog.Any("error", err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("Reconnected to NATS", slog.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			slog.Info("NATS connection closed")
		}),
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, err
	}

	slog.Info("Connected to NATS", slog.String("url", natsURL))

	js, err := nc.JetStream()
	if err != nil {
		return nil, err
	}

	return &NATSClient{Conn: nc, JS: js}, nil
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
}
