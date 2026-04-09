package api

import "encoding/json"

// WebSocket message envelope for agent↔server communication.
// Same pattern as peregrine-ci-scaler/heartbeat/hub.go.

// Envelope wraps all WebSocket messages with a type discriminator.
type Envelope struct {
	Type    string          `json:"type"`
	Ref     string          `json:"ref,omitempty"` // correlation ID for request/response
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Agent → Server message types.
const (
	MsgAgentRegister   = "agent.register"
	MsgAgentUnregister = "agent.unregister"
	MsgAgentNext       = "agent.next"
	MsgHealth          = "health"
	MsgExtend          = "extend"
	MsgWorkflowInit    = "workflow.init"
	MsgWorkflowDone    = "workflow.done"
	MsgStepUpdate      = "step.update"
	MsgStepLog         = "step.log"
)

// Server → Agent message types.
const (
	MsgVersion    = "version"
	MsgRegistered = "registered"
	MsgTaskAssign = "task.assign"
	MsgTaskCancel = "task.cancel"
	MsgAck        = "ack"
)

// Payloads for agent → server messages.

type RegisterPayload struct {
	Platform     string            `json:"platform"`
	Backend      string            `json:"backend"`
	Capacity     int               `json:"capacity"`
	Version      string            `json:"version"`
	CustomLabels map[string]string `json:"custom_labels,omitempty"`
}

type NextPayload struct {
	FilterLabels map[string]string `json:"filter_labels"`
}

type HealthPayload struct {
	WorkflowIDs []string `json:"workflow_ids,omitempty"`
}

type ExtendPayload struct {
	WorkflowIDs []string `json:"workflow_ids"`
}

type InitPayload struct {
	WorkflowID string `json:"workflow_id"`
	Started    int64  `json:"started"`
}

type DonePayload struct {
	WorkflowID string `json:"workflow_id"`
	Started    int64  `json:"started"`
	Finished   int64  `json:"finished"`
	Error      string `json:"error,omitempty"`
	Canceled   bool   `json:"canceled,omitempty"`
}

type UpdatePayload struct {
	WorkflowID string `json:"workflow_id"`
	StepUUID   string `json:"step_uuid"`
	Started    int64  `json:"started,omitempty"`
	Finished   int64  `json:"finished,omitempty"`
	Exited     bool   `json:"exited,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Error      string `json:"error,omitempty"`
	Canceled   bool   `json:"canceled,omitempty"`
}

type LogPayload struct {
	StepUUID string       `json:"step_uuid"`
	Entries  []LogEntryWS `json:"entries"`
}

type LogEntryWS struct {
	Time int64  `json:"time,omitempty"`
	Type int    `json:"type,omitempty"`
	Line int    `json:"line,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// Payloads for server → agent messages.

type VersionPayload struct {
	ServerVersion string `json:"server_version"`
}

type RegisteredPayload struct {
	AgentID int64 `json:"agent_id"`
}

type TaskAssignPayload struct {
	ID      string          `json:"id"`
	Timeout int64           `json:"timeout"`
	Config  json.RawMessage `json:"config"`
}

type TaskCancelPayload struct {
	WorkflowID string `json:"workflow_id"`
}

type AckPayload struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Helper to create an envelope.
func newEnvelope(msgType, ref string, payload interface{}) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(Envelope{Type: msgType, Ref: ref, Payload: raw})
}
