package api

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// TaskStatus represents the current state of a background task.
type TaskStatus string

const (
	TaskNever   TaskStatus = "never"
	TaskIdle    TaskStatus = "idle"
	TaskRunning TaskStatus = "running"
	TaskError   TaskStatus = "error"
)

// Task is a named, schedulable background job that can also be triggered manually.
type Task struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Category     string     `json:"category"` // "library" | "metadata" | "system"
	IntervalStr  string     `json:"interval"` // human-readable, e.g. "30m"
	Status       TaskStatus `json:"status"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastDuration string     `json:"last_duration,omitempty"`
	RunCount     int        `json:"run_count"`

	interval time.Duration
	fn       func(ctx context.Context) error
	mu       sync.Mutex
}

// run executes the task function, updating status fields.
func (t *Task) run(ctx context.Context) {
	t.mu.Lock()
	if t.Status == TaskRunning {
		t.mu.Unlock()
		return // already in progress
	}
	t.Status = TaskRunning
	t.mu.Unlock()

	start := time.Now()
	err := t.fn(ctx)
	elapsed := time.Since(start)

	t.mu.Lock()
	now := time.Now()
	t.LastRun = &now
	t.LastDuration = elapsed.Round(time.Millisecond).String()
	t.RunCount++
	if err != nil {
		t.Status = TaskError
		t.LastError = err.Error()
	} else {
		t.Status = TaskIdle
		t.LastError = ""
	}
	if t.interval > 0 {
		next := now.Add(t.interval)
		t.NextRun = &next
	}
	t.mu.Unlock()
}

// schedule runs the task on its configured interval until ctx is cancelled.
func (t *Task) schedule(ctx context.Context) {
	if t.interval == 0 {
		return // manual-only task
	}
	// Set initial NextRun
	next := time.Now().Add(t.interval)
	t.mu.Lock()
	t.NextRun = &next
	t.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.interval):
			t.run(ctx)
		}
	}
}

// TaskRegistry holds all registered background tasks.
type TaskRegistry struct {
	mu     sync.RWMutex
	tasks  []*Task
	byName map[string]*Task
}

var globalRegistry *TaskRegistry

func SetTaskRegistry(r *TaskRegistry) { globalRegistry = r }

func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{byName: make(map[string]*Task)}
}

// Register adds a task to the registry.
func (r *TaskRegistry) Register(name, description, category, intervalStr string, interval time.Duration, fn func(ctx context.Context) error) *Task {
	t := &Task{
		Name:        name,
		Description: description,
		Category:    category,
		IntervalStr: intervalStr,
		Status:      TaskNever,
		interval:    interval,
		fn:          fn,
	}
	r.mu.Lock()
	r.tasks = append(r.tasks, t)
	r.byName[name] = t
	r.mu.Unlock()
	return t
}

// Start launches all registered tasks on their schedules in background goroutines.
func (r *TaskRegistry) Start(ctx context.Context) {
	r.mu.RLock()
	tasks := make([]*Task, len(r.tasks))
	copy(tasks, r.tasks)
	r.mu.RUnlock()
	for _, t := range tasks {
		go t.schedule(ctx)
	}
}

// RunNow triggers a task immediately in a goroutine (non-blocking).
func (r *TaskRegistry) RunNow(name string) bool {
	r.mu.RLock()
	t, ok := r.byName[name]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	go t.run(context.Background())
	return true
}

// snapshot returns a JSON-serialisable copy of all task states.
func (r *TaskRegistry) snapshot() []taskDTO {
	r.mu.RLock()
	tasks := make([]*Task, len(r.tasks))
	copy(tasks, r.tasks)
	r.mu.RUnlock()

	out := make([]taskDTO, len(tasks))
	for i, t := range tasks {
		t.mu.Lock()
		out[i] = taskDTO{
			Name:         t.Name,
			Description:  t.Description,
			Category:     t.Category,
			Interval:     t.IntervalStr,
			Status:       t.Status,
			LastRun:      t.LastRun,
			NextRun:      t.NextRun,
			LastError:    t.LastError,
			LastDuration: t.LastDuration,
			RunCount:     t.RunCount,
		}
		t.mu.Unlock()
	}
	return out
}

type taskDTO struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Category     string     `json:"category"`
	Interval     string     `json:"interval"`
	Status       TaskStatus `json:"status"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastDuration string     `json:"last_duration,omitempty"`
	RunCount     int        `json:"run_count"`
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// TasksHandler — GET /api/tasks — lists all tasks with their current state.
func TasksHandler(w http.ResponseWriter, r *http.Request) {
	if globalRegistry == nil {
		jsonOK(w, []taskDTO{})
		return
	}
	jsonOK(w, globalRegistry.snapshot())
}

// TaskRunHandler — POST /api/tasks/{name}/run — manually triggers a task.
func TaskRunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if globalRegistry == nil {
		jsonErr(w, "task registry not initialised", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	if !globalRegistry.RunNow(name) {
		jsonErr(w, "unknown task: "+name, http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]string{"status": "triggered", "task": name})
}
