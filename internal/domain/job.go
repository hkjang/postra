package domain

import "context"

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobPartial   JobStatus = "partially_succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
	JobUncertain JobStatus = "uncertain"
)

type Job struct {
	ID        string            `json:"id"`
	UserID    string            `json:"user_id"`
	Type      string            `json:"type"` // "sync" | "analysis" | ...
	AccountID string            `json:"account_id,omitempty"`
	Status    JobStatus         `json:"status"`
	Progress  string            `json:"progress,omitempty"`
	Stats     map[string]int64  `json:"stats,omitempty"`
	Error     string            `json:"error,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	CreatedAt int64             `json:"created_at"`
	UpdatedAt int64             `json:"updated_at"`
}

// SyncStats mirrors POP-013: callers get exact new/dup/failed/oversize/
// parse-failure counts from every run.
type SyncStats struct {
	Seen       int64 `json:"seen"`
	New        int64 `json:"new"`
	Duplicate  int64 `json:"duplicate"`
	Failed     int64 `json:"failed"`
	Oversize   int64 `json:"oversize"`
	ParseError int64 `json:"parse_error"`
}

type AuditEvent struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	UserID   string `json:"user_id"`
	Actor    string `json:"actor"` // "cli" | "rest" | "mcp" | "worker"
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Result   string `json:"result"` // "ok" | "denied" | "error"
	Detail   string `json:"detail,omitempty"`
}

type AuditSink interface {
	Log(ctx context.Context, ev AuditEvent)
}

// GenerationRequest keeps untrusted mail content strictly separated from
// instructions (AI-014): adapters render Untrusted inside a delimited
// data-only block and never as part of the instruction stream.
type GenerationRequest struct {
	System    string
	User      string
	Untrusted string
	JSONMode  bool
	MaxTokens int
	// Task, when set, selects a per-task model/endpoint route configured in
	// AIConfig.TaskModels. Unknown/empty tasks use the default endpoint.
	Task string
}

type GenerationResult struct {
	Text      string
	Model     string
	InputHash string
}

type EmbeddingRequest struct {
	Input []string
}

type EmbeddingResult struct {
	Vectors [][]float32
	Model   string
}

type EmbeddingItem struct {
	MessageID string
	ChunkID   int
	Vector    []float32
	Model     string
}

type AIProvider interface {
	Generate(ctx context.Context, req GenerationRequest) (GenerationResult, error)
	// Embed returns one vector per input string. Used for semantic search.
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResult, error)
}

// SemanticHit is a semantic-search result: a message and its similarity to
// the query (1.0 = identical direction).
type SemanticHit struct {
	MessageID string  `json:"message_id"`
	Score     float64 `json:"score"`
}

type Analysis struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	TargetType    string `json:"target_type"` // "message" | "thread"
	TargetID      string `json:"target_id"`
	AnalysisType  string `json:"analysis_type"`
	ResultJSON    string `json:"result_json"`
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	InputHash     string `json:"input_hash"`
	CreatedAt     int64  `json:"created_at"`
}
