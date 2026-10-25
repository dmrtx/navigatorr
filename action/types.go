package action

import (
	"context"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Action statuses mirroring store constants
const (
	StatusPending         = store.ActionStatusPending
	StatusRunning         = store.ActionStatusRunning
	StatusWaitingExternal = store.ActionStatusWaitingExternal
	StatusWaitingDecision = store.ActionStatusWaitingDecision
	StatusCompleted       = store.ActionStatusCompleted
	StatusFailed          = store.ActionStatusFailed
	StatusCancelled       = store.ActionStatusCancelled
)

// StepStatus indicates the outcome of an individual action step
type StepStatus string

const (
	StepCompleted       StepStatus = "completed"
	StepWaitingExternal StepStatus = "waiting_external"
	StepWaitingDecision StepStatus = "waiting_decision"
	StepFailed          StepStatus = "failed"
	StepSkipped         StepStatus = "skipped"
	StepCancelled       StepStatus = "cancelled"
)

// WaitingOption represents a choice presented to the LLM during waiting_decision
type WaitingOption struct {
	Decision    string `json:"decision"`
	Description string `json:"description"`
}

// StepResult is returned by each step execution
type StepResult struct {
	Status           StepStatus      `json:"status"`
	Outputs          map[string]any  `json:"outputs,omitempty"`
	WaitingReason    string          `json:"waiting_reason,omitempty"`
	WaitingCondition string          `json:"waiting_condition,omitempty"`
	WaitingOptions   []WaitingOption `json:"waiting_options,omitempty"`
	Error            string          `json:"error,omitempty"`
	// RewindTo names a step of the template the action continues from after
	// this completed step, instead of the next one. It is how an action repeats
	// its search; the step that sets it has already reset the state it repeats.
	RewindTo string `json:"rewind_to,omitempty"`
}

// ExecutionContext carries live state, inputs, outputs, and dependencies across steps
type ExecutionContext struct {
	InstanceID  string         `json:"instance_id"`
	ActionName  string         `json:"action_name"`
	Inputs      map[string]any `json:"inputs"`
	State       map[string]any `json:"state"`
	Outputs     map[string]any `json:"outputs"`
	Decision    string         `json:"decision,omitempty"`
	ExtraInputs map[string]any `json:"extra_inputs,omitempty"`

	// Dependencies
	Engine *Engine `json:"-"`
}

// StepDefinition defines a single step in a declarative action workflow
type StepDefinition struct {
	Name        string
	Description string
	Run         func(ctx context.Context, ec *ExecutionContext) (StepResult, error)
}

// ActionTemplate defines a declarative workflow composed of sequential steps
type ActionTemplate struct {
	Name            string
	Version         int
	Description     string
	Examples        []ActionExample
	RequiredInputs  []string
	OptionalInputs  []string
	Destructive     bool
	ImmutableInputs bool
	// AutoReconcile opts this workflow into autonomous continuation of external waits.
	// Decision waits are always excluded.
	AutoReconcile bool
	Steps         []StepDefinition
	// Validate, when set, judges a request before anything is persisted. Every
	// path that creates an action (Enqueue, EnqueueWorker, Run) calls it before
	// it looks up or reserves an idempotency key, checks worker admission or
	// writes a row, so a bad request leaves no trace and binds no key. It
	// returns the inputs to store (nil means inputs unchanged) or an
	// *InvalidParamsError naming the input to fix. It must not read media or
	// contact a worker. Preflight repeats the same checks defensively.
	Validate func(ctx context.Context, e *Engine, inputs map[string]any) (map[string]any, error)
}

// Inputs uses the same JSON string format accepted by action_run.
type ActionExample struct {
	Description string `json:"description"`
	Inputs      string `json:"inputs"`
}

// ActionCatalogEntry describes an action workflow definition for discovery
type ActionCatalogEntry struct {
	Name            string          `json:"name"`
	Version         int             `json:"version"`
	Description     string          `json:"description"`
	Examples        []ActionExample `json:"examples,omitempty"`
	RequiredInputs  []string        `json:"required_inputs"`
	OptionalInputs  []string        `json:"optional_inputs"`
	Steps           []string        `json:"steps"`
	Destructive     bool            `json:"destructive"`
	ImmutableInputs bool            `json:"immutable_inputs"`
}

// ActionResult is returned when running, resuming, or querying an action
type ActionResult struct {
	ID                   string          `json:"id"`
	ActionName           string          `json:"action_name"`
	Status               string          `json:"status"`
	CurrentStep          int             `json:"current_step"`
	TotalSteps           int             `json:"total_steps"`
	Inputs               map[string]any  `json:"inputs"`
	Outputs              map[string]any  `json:"outputs"`
	State                map[string]any  `json:"state"`
	WaitingReason        string          `json:"waiting_reason,omitempty"`
	WaitingCondition     string          `json:"waiting_condition,omitempty"`
	WaitingOptions       []WaitingOption `json:"waiting_options,omitempty"`
	Error                string          `json:"error,omitempty"`
	IdempotencyKey       string          `json:"idempotency_key,omitempty"`
	DurationMs           int64           `json:"duration_ms"` // Deprecated alias of wall_duration_ms.
	WallDurationMs       int64           `json:"wall_duration_ms"`
	QueueDurationMs      *int64          `json:"queue_duration_ms,omitempty"`
	EncodeDurationMs     *int64          `json:"encode_duration_ms,omitempty"`
	ValidationDurationMs *int64          `json:"validation_duration_ms,omitempty"`
	ReconcileLagMs       *int64          `json:"reconcile_lag_ms,omitempty"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`
}

// EngineDeps bundles dependencies needed by the Action Engine
type EngineDeps struct {
	Store    *store.Store
	Config   *config.Config
	Registry *arrservice.Registry
	Qbit     *qbit.Client
	Fs       *fsop.Resolver
	Ffprobe  string
	// NASHelper, when set, hashes promotion files where they live instead of
	// reading them through the mount; nil keeps every read on the mount.
	NASHelper *nashelper.Client
	// DrainSettle overrides how long a new drain is reported not ready (zero
	// keeps the default; negative means no wait). Tests use it.
	DrainSettle time.Duration
	// Workers are the transcode workers, by name. Every worker call goes to
	// the worker an action was assigned (see worker_affinity.go).
	Workers *transcode.WorkerSet
	// Transcode is shorthand for one worker named
	// transcode.DefaultWorkerName. When set, NewEngine replaces Workers with
	// it and clears it, so the engine itself only ever reads Workers.
	Transcode transcode.Executor
	StartTime time.Time
	// Now and ReconcileInterval are optional clock/scheduling overrides.
	Now               func() time.Time
	ReconcileInterval time.Duration
}
