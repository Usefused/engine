//go:build fused_agent_release

package agentbundle

import _ "embed"

// Releases must embed freshly verified CI inputs; missing staging fails compilation.
//
//go:embed release-assets/agent.tar.gz
var artifact []byte

//go:embed release-assets/runtimes.json
var runtimeManifest []byte
