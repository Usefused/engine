package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/Usefused/engine/internal/engine/mcpsession"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// initialMCPSessionMetadata records the transport peer once; later requests cannot rewrite its provenance.
func initialMCPSessionMetadata(request *http.Request) mcpsession.Metadata {
	return mcpsession.Metadata{InitialClientIP: mcpClientIP(request, os.Getenv("FUSED_MCP_TRUSTED_PROXY_CIDRS"))}
}

// mcpClientIP trusts forwarding only across an explicitly configured, bounded proxy chain.
func mcpClientIP(request *http.Request, configured string) string {
	peer := mcpPeerAddress(request.RemoteAddr)
	// A malformed transport peer cannot grant forwarding authority.
	if !peer.IsValid() {
		return ""
	}
	trusted := mcpTrustedProxyPrefixes(configured)
	// Untrusted senders can forge forwarding headers; their socket address remains authoritative.
	if !mcpTrustedProxy(peer, trusted) {
		return peer.String()
	}
	forwarded := request.Header.Values("X-Forwarded-For")
	chain := strings.Join(forwarded, ",")
	// Oversized chains fail closed without copying arbitrary request metadata into history.
	if len(chain) > 4096 {
		return peer.String()
	}
	address, ok := mcpForwardedPeer(chain, peer, trusted)
	// Invalid proxy evidence never replaces the directly observed peer.
	if !ok {
		return peer.String()
	}
	return address.String()
}

// mcpPeerAddress accepts only literal transport addresses and discards IPv6 zone identifiers.
func mcpPeerAddress(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	// Tests and non-TCP adapters may provide a bare address, but never a DNS name.
	if err != nil {
		host = remote
	}
	address, err := netip.ParseAddr(host)
	// Invalid transport metadata is absent rather than echoed as a claimed address.
	if err != nil {
		return netip.Addr{}
	}
	return address.WithZone("").Unmap()
}

// mcpTrustedProxyPrefixes treats malformed configuration as no trust, never a partial allowlist.
func mcpTrustedProxyPrefixes(configured string) []netip.Prefix {
	parts := strings.Split(configured, ",")
	// Configuration remains bounded even when inherited from an operator environment.
	if len(parts) > 32 || len(configured) > 4096 {
		return nil
	}
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		// One invalid entry disables forwarding trust rather than broadening the accepted chain.
		if err != nil {
			return nil
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// mcpTrustedProxy checks a literal address against the operator-owned allowlist only.
func mcpTrustedProxy(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		// The first containing network establishes authority for this hop only.
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// mcpForwardedPeer walks right to left so attacker-supplied leftmost claims cannot skip an untrusted hop.
func mcpForwardedPeer(chain string, peer netip.Addr, trusted []netip.Prefix) (netip.Addr, bool) {
	parts := strings.Split(chain, ",")
	// Each hop consumes a bounded amount of parsing work.
	if len(parts) > 32 {
		return peer, false
	}
	current := peer
	for index := len(parts) - 1; index >= 0; index-- {
		// Stop at the first peer whose forwarding claims have no configured authority.
		if !mcpTrustedProxy(current, trusted) {
			break
		}
		address, err := netip.ParseAddr(strings.TrimSpace(parts[index]))
		// Zone identifiers and invalid literals are not portable client-IP evidence.
		if err != nil || address.Zone() != "" {
			return peer, false
		}
		current = address.Unmap()
	}
	return current, true
}

// captureMCPClientInfo records the first initialize claim without treating it as verified identity.
func captureMCPClientInfo(ctx context.Context, request mcpJSONRPCRequest, session *mcpSession) {
	// Initialization metadata and its durable event must not overtake a concurrent ended transition.
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	captureMCPClientInfoLocked(ctx, request, session)
}

// captureMCPClientInfoLocked records the bounded client claim without declaring protocol initialization complete.
func captureMCPClientInfoLocked(ctx context.Context, request mcpJSONRPCRequest, session *mcpSession) {
	// Tool arguments and ordinary protocol messages must never become session metadata.
	if request.Method != "initialize" {
		return
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.sandbox.mcp.initialize_metadata")
	defer span.End()
	span.SetAttributes(attribute.String("outcome", "invalid"))
	session.activityMu.Lock()
	ended := session.ended
	session.activityMu.Unlock()
	// A late initialize claim cannot add metadata or an event after the session was retired.
	if ended {
		span.SetAttributes(attribute.String("outcome", "session_ended"))
		return
	}
	claim, ok := mcpInitializeClientClaim(request)
	// Malformed or oversized claims remain absent rather than changing protocol admission behavior.
	if !ok {
		return
	}
	session.metadataMu.Lock()
	// SSE starts before initialize; only the first valid initialization may fill its client claim.
	if session.clientInfoRecorded {
		session.metadataMu.Unlock()
		span.SetAttributes(attribute.String("outcome", "already_recorded"))
		return
	}
	session.clientInfoRecorded = true
	session.clientMetadata.ClientName, session.clientMetadata.ClientVersion = claim.ClientName, claim.ClientVersion
	session.metadataMu.Unlock()
	span.SetAttributes(attribute.String("outcome", "recorded"))
}

// commitMCPInitializationLocked publishes the single initialized transition after a valid child result.
func commitMCPInitializationLocked(session *mcpSession, protocolVersion string) bool {
	// Repeated or delayed child responses cannot duplicate the durable lifecycle transition.
	if session.initialized {
		return false
	}
	session.activityMu.Lock()
	session.protocolVersion = protocolVersion
	session.activityMu.Unlock()
	session.initialized = true
	publishMCPSessionEventLocked(session, "initialized", "")
	return true
}

// mcpInitializeClientClaim admits present display claims only from a correlated initialization request.
func mcpInitializeClientClaim(request mcpJSONRPCRequest) (mcpsession.Metadata, bool) {
	requestID := strings.TrimSpace(string(request.ID))
	// Notifications and null IDs cannot consume the one-time claim before the real handshake arrives.
	if requestID == "" || requestID == "null" {
		return mcpsession.Metadata{}, false
	}
	var params struct {
		ClientInfo *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	// Absence or JSON null is unknown metadata, not an authoritative empty claim.
	if json.Unmarshal(request.Params, &params) != nil || params.ClientInfo == nil {
		return mcpsession.Metadata{}, false
	}
	claim := mcpsession.Metadata{ClientName: params.ClientInfo.Name, ClientVersion: params.ClientInfo.Version}
	return claim, strings.TrimSpace(claim.ClientName) != "" && claim.Valid()
}

// mcpSessionMetadata snapshots display-only provenance without racing lifecycle publication.
func mcpSessionMetadata(session *mcpSession) mcpsession.Metadata {
	session.metadataMu.Lock()
	defer session.metadataMu.Unlock()
	return session.clientMetadata
}
