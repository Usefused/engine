//go:build !fused_agent_release

package agentbundle

import _ "embed"

// Local builds retain checked-in inputs without requiring the native release matrix.
//
//go:embed assets/agent.tar.gz
var artifact []byte

//go:embed assets/runtimes.json
var runtimeManifest []byte
