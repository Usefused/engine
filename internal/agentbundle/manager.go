package agentbundle

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Status struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Manager owns one isolated agent process. Constructing it never installs or starts anything.
type Manager struct {
	options     Options
	mu          sync.RWMutex
	status      Status
	endpoint    string
	transport   *http.Transport
	cancel      context.CancelFunc
	done        chan struct{}
	startOnce   sync.Once
	diagnostics string
}

// New prepares a supervisor without starting processes or performing network downloads.
func New(options Options) (*Manager, error) {
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if options.EngineURL == "" {
		return nil, errors.New("local agent requires an Engine URL")
	}
	return &Manager{options: options, status: Status{State: "starting", Message: "Starting Fused Agent"}, done: make(chan struct{})}, nil
}

// DiagnosticsPath returns the local diagnostics path under synchronization, never its potentially private content.
func (m *Manager) DiagnosticsPath() string { m.mu.RLock(); defer m.mu.RUnlock(); return m.diagnostics }

// Status returns a consistent startup snapshot without blocking Engine request handling.
func (m *Manager) Status() Status { m.mu.RLock(); defer m.mu.RUnlock(); return m.status }

// Endpoint exposes the child connection only after its pinned TLS readiness probe succeeds.
func (m *Manager) Endpoint() (string, http.RoundTripper) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Only a healthy child may remain available to authenticated callers.
	if m.status.State != "ready" {
		return "", nil
	}
	return m.endpoint, m.transport
}

// set revokes stale endpoints whenever the child leaves its ready state.
func (m *Manager) set(state, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = Status{state, message}
	// Only a healthy child may remain available to authenticated callers.
	if state != "ready" {
		m.endpoint = ""
	}
}

// Start runs installation and startup in the background, independently of Engine readiness.
func (m *Manager) Start(parent context.Context) {
	m.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		m.mu.Lock()
		m.cancel = cancel
		m.mu.Unlock()
		go func() { defer close(m.done); m.run(ctx) }()
	})
}

// Close cancels the owned process and waits for cleanup before releasing its supervisor.
func (m *Manager) Close() error {
	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if cancel == nil {
		return nil
	}
	cancel()
	<-m.done
	return nil
}

// run isolates runtime installation and bounded restart attempts from Engine availability.
func (m *Manager) run(ctx context.Context) {
	root, err := InstallRuntime(ctx, m.options)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		m.set("unavailable", err.Error())
		return
	}
	entry, err := runtimeSpec()
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		m.set("unavailable", err.Error())
		return
	}
	work, err := os.MkdirTemp("", "fused-agent-")
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		m.set("unavailable", "Unable to prepare Fused Agent")
		return
	}
	defer os.RemoveAll(work)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err = extractAgent(ctx, work); err != nil {
		m.set("unavailable", "Unable to unpack bundled Fused Agent")
		return
	}
	transport, err := localTLS(work)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		m.set("unavailable", "Unable to secure local agent connection")
		return
	}
	defer transport.CloseIdleConnections()
	for attempt := 0; attempt < 3; attempt++ {
		// Cancellation must release the owned resources rather than leave background work running.
		if ctx.Err() != nil {
			m.set("unavailable", "Fused Agent stopped")
			return
		}
		m.set("starting", "Starting Fused Agent")
		err = m.serve(ctx, root, entry, work, transport)
		// Cancellation must release the owned resources rather than leave background work running.
		if ctx.Err() != nil {
			m.set("unavailable", "Fused Agent stopped")
			return
		}
		m.set("unavailable", fmt.Sprintf("Fused Agent stopped: %v", err))
		// Keep process and credential ownership intact when this operation cannot complete normally.
		if attempt < 2 {
			select {
			// Cancellation must release the owned resources rather than leave background work running.
			case <-ctx.Done():
				return
			// Keep process and credential ownership intact when this operation cannot complete normally.
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}
}

// serve owns the child process, private logs and readiness deadline through cancellation or exit.
func (m *Manager) serve(ctx context.Context, root string, entry runtimeEntry, work string, transport *http.Transport) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	endpoint := "https://127.0.0.1:" + strconv.Itoa(port)
	command := exec.Command(filepath.Join(root, filepath.FromSlash(entry.Python)), "-I", filepath.Join(work, "launch.py"), filepath.Join(root, "packages"), work, strconv.Itoa(port), filepath.Join(work, "local.crt"), filepath.Join(work, "local.key"))
	command.Dir = work
	command.Env = childEnvironment(m.options)
	stdin, err := command.StdinPipe()
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return err
	}
	// Keep bounded diagnostics private to this installation, separate from shared runtime files.
	cache, err := cacheRoot(m.options)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		stdin.Close()
		return err
	}
	logs := filepath.Join(cache, "logs")
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err = os.MkdirAll(logs, 0700); err != nil {
		stdin.Close()
		return err
	}
	log, err := os.CreateTemp(logs, "agent-*.log")
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		stdin.Close()
		return err
	}
	defer log.Close()
	m.mu.Lock()
	m.diagnostics = log.Name()
	m.mu.Unlock()
	output := &boundedLog{writer: log, remaining: 1 << 20}
	command.Stdout = output
	command.Stderr = output
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err = command.Start(); err != nil {
		stdin.Close()
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	defer func() { stdin.Close(); command.Process.Kill() }()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ready := false
	for {
		select {
		// Cancellation must release the owned resources rather than leave background work running.
		case <-ctx.Done():
			stdin.Close()
			select {
			// Keep process and credential ownership intact when this operation cannot complete normally.
			case <-exited:
			// Keep process and credential ownership intact when this operation cannot complete normally.
			case <-time.After(5 * time.Second):
				command.Process.Kill()
				<-exited
			}
			return ctx.Err()
		// Keep process and credential ownership intact when this operation cannot complete normally.
		case err := <-exited:
			return fmt.Errorf("agent process exited: %v", err)
		// Keep process and credential ownership intact when this operation cannot complete normally.
		case <-deadline.C:
			command.Process.Kill()
			<-exited
			return errors.New("agent startup timed out")
		// Keep process and credential ownership intact when this operation cannot complete normally.
		case <-tick.C:
			// Only a healthy child may remain available to authenticated callers.
			if ready {
				continue
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
			response, err := client.Do(request)
			// Keep process and credential ownership intact when this operation cannot complete normally.
			if err != nil {
				continue
			}
			response.Body.Close()
			// Keep process and credential ownership intact when this operation cannot complete normally.
			if response.StatusCode == http.StatusOK {
				ready = true
				deadline.Stop()
				m.mu.Lock()
				m.endpoint = endpoint
				m.transport = transport
				m.status = Status{State: "ready"}
				m.mu.Unlock()
			}
		}
	}
}

// childEnvironment exposes only runtime essentials and explicit model/identity configuration to the bundled agent.
func childEnvironment(options Options) []string {
	overrides := map[string]string{
		"FUSED_AGENT_GATEWAY_URL":                            options.GatewayURL,
		"FUSED_AGENT_GATEWAY_KEY":                            options.GatewayKey,
		"FUSED_AGENT_MODEL":                                  options.Model,
		"FUSED_AGENT_IDENTITY_KEY":                           options.IdentityKey,
		"FUSED_AGENT_REGISTRY_GATEWAY":                       options.RegistryGateway,
		"PYTHONDONTWRITEBYTECODE":                            "1",
		"LITELLM_LOCAL_MODEL_COST_MAP":                       "True",
		"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "NO_CONTENT",
		"ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS":               "false",
	}
	env := make([]string, 0, len(overrides)+5)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "SYSTEMROOT", "WINDIR"} {
		// Runtime path resolution needs these OS values, never the parent Engine's database or encryption credentials.
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

// A per-process certificate pins the child identity before any user credential is sent.
func localTLS(directory string) (*http.Transport, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Fused local agent"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(10, 0, 0), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err != nil {
		return nil, err
	}
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err = os.WriteFile(filepath.Join(directory, "local.crt"), certPEM, 0600); err != nil {
		return nil, err
	}
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if err = os.WriteFile(filepath.Join(directory, "local.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 120 * time.Second, IdleConnTimeout: 90 * time.Second}, nil
}

// boundedLog bounds diagnostic storage without blocking or exposing output to Engine clients.
type boundedLog struct {
	mu        sync.Mutex
	writer    io.Writer
	remaining int
}

// Write bounds private diagnostics without blocking the child after the log budget is exhausted.
func (w *boundedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	length := len(p)
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	// Keep process and credential ownership intact when this operation cannot complete normally.
	if len(p) > 0 {
		n, err := w.writer.Write(p)
		w.remaining -= n
		// Keep process and credential ownership intact when this operation cannot complete normally.
		if err != nil {
			return n, err
		}
	}
	return length, nil
}
