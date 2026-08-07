package router

// WorkflowIDHeader is an optional trusted client hint that groups independent
// conversations (for example subagents) for placement affinity only.
const WorkflowIDHeader = "X-Octopus-Workflow-ID"

// RequestMetadata contains transport and workflow facts that are not part of
// harness' provider-neutral ChatRequest. Servers should construct it from the
// decoded inbound request and trusted Octopus headers; it must never be
// forwarded to an upstream model.
type RequestMetadata struct {
	Endpoint   string
	Stream     bool
	WorkflowID string
	// Policy overrides are trusted control-plane inputs. The transport must
	// accept them only from an authenticated caller. They can narrow normal
	// routing but never bypass capabilities or data-placement policy.
	MinQuality     float64
	FixedModel     string
	HighestQuality bool
}
