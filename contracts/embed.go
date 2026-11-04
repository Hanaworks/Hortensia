// Package contracts provides the protocol schema shipped with the executable.
package contracts

import _ "embed"

//go:embed agent-v1.schema.json
var agentV1Schema string

// AgentV1Schema returns a private copy of the bundled schema. Its .invalid ID is
// a namespace, not a URL to fetch at runtime.
func AgentV1Schema() []byte { return []byte(agentV1Schema) }
