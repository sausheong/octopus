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
}
