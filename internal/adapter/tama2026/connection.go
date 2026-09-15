package tama2026

import (
	"encoding/json"

	"github.com/kritama/tama-link/internal/catalog"
)

// Connection is a verified handle to one profile's upstream: the discovery
// snapshot plus the effective catalog. It is the only object that can
// execute operations for the profile.
type Connection struct {
	adapter            *Adapter
	protocolVersion    string
	serverName         string
	serverVersion      string
	serverCapabilities json.RawMessage
	taskExtension      bool
	catalog            catalog.Catalog
}

// Snapshot is the persistence-ready discovery record for an accepted
// submission. It contains no upstream payloads and no credentials.
type Snapshot struct {
	// ProtocolVersion is the negotiated MCP protocol version.
	ProtocolVersion string `json:"protocol_version"`
	// ServerName and ServerVersion identify the upstream server.
	ServerName    string `json:"server_name"`
	ServerVersion string `json:"server_version"`
	// ServerCapabilities is the raw discovered capability object.
	ServerCapabilities json.RawMessage `json:"server_capabilities"`
	// AdapterVersion identifies the Tama Link adapter that verified this
	// connection.
	AdapterVersion string `json:"adapter_version"`
}

// Snapshot returns the persistence-ready discovery record.
func (cn *Connection) Snapshot() Snapshot {
	return Snapshot{
		ProtocolVersion:    cn.protocolVersion,
		ServerName:         cn.serverName,
		ServerVersion:      cn.serverVersion,
		ServerCapabilities: cn.serverCapabilities,
		AdapterVersion:     cn.adapter.adapterVersion,
	}
}

// Catalog returns the effective (pinned, live-verified) catalog.
func (cn *Connection) Catalog() catalog.Catalog { return cn.catalog }

// HasTaskExtension reports whether the upstream declared the Tasks
// extension for this connection.
func (cn *Connection) HasTaskExtension() bool { return cn.taskExtension }
