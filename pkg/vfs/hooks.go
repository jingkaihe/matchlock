package vfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type HookOp string

const (
	HookOpStat      HookOp = "stat"
	HookOpReadDir   HookOp = "readdir"
	HookOpOpen      HookOp = "open"
	HookOpCreate    HookOp = "create"
	HookOpMkdir     HookOp = "mkdir"
	HookOpChmod     HookOp = "chmod"
	HookOpRemove    HookOp = "remove"
	HookOpRemoveAll HookOp = "remove_all"
	HookOpRename    HookOp = "rename"
	HookOpSymlink   HookOp = "symlink"
	HookOpReadlink  HookOp = "readlink"
	HookOpRead      HookOp = "read"
	HookOpWrite     HookOp = "write"
	HookOpClose     HookOp = "close"
	HookOpSync      HookOp = "sync"
	HookOpTruncate  HookOp = "truncate"
)

type HookPhase string

const (
	HookPhaseBefore HookPhase = "before"
	HookPhaseAfter  HookPhase = "after"
)

type HookAction string

const (
	HookActionAllow       HookAction = "allow"
	HookActionBlock       HookAction = "block"
	HookActionMutateWrite HookAction = "mutate_write"
	HookActionExecAfter   HookAction = "exec_after"
)

type HookRule struct {
	Name        string
	Phase       HookPhase
	Ops         []HookOp
	PathPattern string
	Action      HookAction
	ActionFunc  func(ctx context.Context, req HookRequest) HookAction

	MutateWriteFunc MutateWriteFunc
	MutateWrite     []byte

	ExecCommand string
	ExecTimeout time.Duration
}

type HookRequest struct {
	Op      HookOp
	Path    string
	NewPath string
	Flags   int
	Mode    os.FileMode
	UID     int
	GID     int
	Offset  int64
	Data    []byte
}

type HookFileMeta struct {
	Size         int64
	Mode         os.FileMode
	UID          int
	GID          int
	HasOwnership bool
}

type HookResult struct {
	Err   error
	Bytes int
	Meta  *HookFileMeta
}

type ExecFunc func(ctx context.Context, command string) error

// MutateWriteFunc computes replacement bytes for a write operation.
// Returning an error fails the intercepted write.
type MutateWriteFunc func(ctx context.Context, req MutateWriteRequest) ([]byte, error)

// MutateWriteRequest contains metadata for write mutation decisions.
type MutateWriteRequest struct {
	Path   string
	Offset int64
	Size   int
	Mode   os.FileMode
	UID    int
	GID    int
}

// HookMatcher decides whether a hook should apply for a request.
type HookMatcher interface {
	Match(req *HookRequest) bool
}

// HookMatcherFunc adapts a function into HookMatcher.
type HookMatcherFunc func(req *HookRequest) bool

func (f HookMatcherFunc) Match(req *HookRequest) bool {
	if f == nil {
		return true
	}
	return f(req)
}

// OpPathMatcher matches by operation and filepath-style glob pattern.
type OpPathMatcher struct {
	Ops         []HookOp
	PathPattern string
}

func (m OpPathMatcher) Match(req *HookRequest) bool {
	return matchesOpPath(req, m.Ops, m.PathPattern)
}

// BeforeHookCallback runs inline before the target VFS operation.
type BeforeHookCallback interface {
	Before(ctx context.Context, req *HookRequest) error
}

// BeforeHookFunc adapts a function into BeforeHookCallback.
type BeforeHookFunc func(ctx context.Context, req *HookRequest) error

func (f BeforeHookFunc) Before(ctx context.Context, req *HookRequest) error {
	if f == nil {
		return nil
	}
	return f(ctx, req)
}

// AfterHookCallback runs after the target VFS operation.
type AfterHookCallback interface {
	After(ctx context.Context, req HookRequest, result HookResult)
}

// AfterHookFunc adapts a function into AfterHookCallback.
type AfterHookFunc func(ctx context.Context, req HookRequest, result HookResult)

func (f AfterHookFunc) After(ctx context.Context, req HookRequest, result HookResult) {
	if f == nil {
		return
	}
	f(ctx, req, result)
}

// Hook represents a callback-driven interception hook.
type Hook struct {
	Name    string
	Phase   HookPhase
	Matcher HookMatcher

	Before BeforeHookCallback
	After  AfterHookCallback

	// Async enqueues after-callback execution on the hook worker.
	Async bool
	// SideEffect marks callbacks that should be suppressed while hook-triggered
	// side effects are in-flight.
	SideEffect bool
}

type hookTask struct {
	hook   Hook
	req    HookRequest
	result HookResult
}

type HookEngine struct {
	hooks []Hook

	execMu sync.RWMutex
	execFn ExecFunc

	eventMu sync.RWMutex
	eventFn func(req HookRequest, result HookResult)

	maxExecDepth    int32
	sideEffectDepth atomic.Int32

	queue   chan hookTask
	closed  atomic.Bool
	closeWg sync.WaitGroup
	tasksWg sync.WaitGroup
	once    sync.Once
}

// NewHookEngine constructs a hook engine from declarative rules.
// Rules are compiled into callback hooks internally.
func NewHookEngine(rules []HookRule, maxExecDepth int) *HookEngine {
	h := newHookEngine(maxExecDepth)
	h.hooks = compileHooksFromRules(rules, h.execFunc)
	return h
}

// NewHookEngineWithCallbacks constructs a hook engine from callback hooks.
func NewHookEngineWithCallbacks(hooks []Hook, maxExecDepth int) *HookEngine {
	h := newHookEngine(maxExecDepth)
	h.hooks = normalizeHooks(hooks)
	return h
}

func newHookEngine(maxExecDepth int) *HookEngine {
	if maxExecDepth <= 0 {
		maxExecDepth = 1
	}

	h := &HookEngine{
		maxExecDepth: int32(maxExecDepth),
		queue:        make(chan hookTask, 128),
	}

	h.closeWg.Add(1)
	go h.run()

	return h
}

func (h *HookEngine) SetExecFunc(fn ExecFunc) {
	if h == nil {
		return
	}
	h.execMu.Lock()
	h.execFn = fn
	h.execMu.Unlock()
}

func (h *HookEngine) SetEventFunc(fn func(req HookRequest, result HookResult)) {
	if h == nil {
		return
	}
	h.eventMu.Lock()
	h.eventFn = fn
	h.eventMu.Unlock()
}

func (h *HookEngine) Before(req *HookRequest) error {
	if h == nil || req == nil {
		return nil
	}

	if req.Path != "" {
		req.Path = filepath.Clean(req.Path)
	}
	if req.NewPath != "" {
		req.NewPath = filepath.Clean(req.NewPath)
	}

	for _, hook := range h.hooks {
		if hook.Phase != HookPhaseBefore || hook.Before == nil {
			continue
		}
		if !hookMatches(hook, req) {
			continue
		}
		if err := hook.Before.Before(context.Background(), req); err != nil {
			return err
		}
	}

	return nil
}

func (h *HookEngine) After(req HookRequest, result HookResult) {
	if h == nil || h.closed.Load() {
		return
	}

	for _, hook := range h.hooks {
		if hook.Phase != HookPhaseAfter || hook.After == nil {
			continue
		}
		if !hookMatches(hook, &req) {
			continue
		}
		if hook.SideEffect && h.sideEffectDepth.Load() > 0 {
			continue
		}

		if hook.Async {
			h.tasksWg.Add(1)
			task := hookTask{hook: hook, req: req, result: result}
			select {
			case h.queue <- task:
			default:
				h.tasksWg.Done()
			}
			continue
		}

		hook.After.After(context.Background(), req, result)
	}

	h.emitEvent(req, result)
}

func (h *HookEngine) Wait() {
	if h == nil {
		return
	}
	h.tasksWg.Wait()
}

func (h *HookEngine) Close() {
	if h == nil {
		return
	}

	h.once.Do(func() {
		h.closed.Store(true)
		h.tasksWg.Wait()
		close(h.queue)
		h.closeWg.Wait()
	})
}

func (h *HookEngine) run() {
	defer h.closeWg.Done()

	for task := range h.queue {
		h.runTask(task)
		h.tasksWg.Done()
	}
}

func (h *HookEngine) runTask(task hookTask) {
	if task.hook.SideEffect {
		if h.sideEffectDepth.Load() >= h.maxExecDepth {
			return
		}
		h.sideEffectDepth.Add(1)
		defer h.sideEffectDepth.Add(-1)
	}

	task.hook.After.After(context.Background(), task.req, task.result)
}

func (h *HookEngine) execFunc() ExecFunc {
	h.execMu.RLock()
	defer h.execMu.RUnlock()
	return h.execFn
}

func (h *HookEngine) emitEvent(req HookRequest, result HookResult) {
	h.eventMu.RLock()
	fn := h.eventFn
	h.eventMu.RUnlock()
	if fn == nil {
		return
	}
	fn(req, result)
}

func compileHooksFromRules(rules []HookRule, getExec func() ExecFunc) []Hook {
	normalized := make([]HookRule, 0, len(rules))
	for _, rule := range rules {
		rule.Phase = normalizeHookPhase(rule.Phase)
		rule.Action = normalizeHookAction(rule.Action)
		normalized = append(normalized, rule)
	}

	hooks := make([]Hook, 0, len(normalized))
	for _, rule := range normalized {
		hook := Hook{
			Name:    rule.Name,
			Phase:   rule.Phase,
			Matcher: OpPathMatcher{Ops: append([]HookOp(nil), rule.Ops...), PathPattern: rule.PathPattern},
		}

		if rule.ActionFunc != nil {
			actionFunc := rule.ActionFunc
			hook.Before = BeforeHookFunc(func(ctx context.Context, req *HookRequest) error {
				if req == nil {
					return nil
				}
				decision := normalizeHookAction(actionFunc(ctx, *req))
				if decision == HookActionBlock {
					return syscall.EPERM
				}
				return nil
			})
			hooks = append(hooks, hook)
			continue
		}

		switch rule.Action {
		case HookActionBlock:
			hook.Before = BeforeHookFunc(func(ctx context.Context, req *HookRequest) error {
				return syscall.EPERM
			})
		case HookActionMutateWrite:
			mutateFn := rule.MutateWriteFunc
			data := append([]byte(nil), rule.MutateWrite...)
			hook.Before = BeforeHookFunc(func(ctx context.Context, req *HookRequest) error {
				if req == nil || req.Op != HookOpWrite {
					return nil
				}
				if mutateFn != nil {
					mutateReq := MutateWriteRequest{
						Path:   req.Path,
						Offset: req.Offset,
						Size:   len(req.Data),
						Mode:   req.Mode,
						UID:    req.UID,
						GID:    req.GID,
					}
					mutated, err := mutateFn(ctx, mutateReq)
					if err != nil {
						return err
					}
					req.Data = append([]byte(nil), mutated...)
					return nil
				}
				if len(data) > 0 {
					req.Data = append([]byte(nil), data...)
				}
				return nil
			})
		case HookActionExecAfter:
			if rule.ExecCommand == "" {
				continue
			}
			command := rule.ExecCommand
			timeout := rule.ExecTimeout
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			hook.After = AfterHookFunc(func(ctx context.Context, req HookRequest, result HookResult) {
				if getExec == nil {
					return
				}
				fn := getExec()
				if fn == nil {
					return
				}
				execCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				_ = fn(execCtx, command)
			})
			hook.Async = true
			hook.SideEffect = true
		case HookActionAllow:
			// Explicit allow is currently a no-op.
			continue
		default:
			continue
		}

		if hook.Before != nil || hook.After != nil {
			hooks = append(hooks, hook)
		}
	}

	return normalizeHooks(hooks)
}

func normalizeHooks(hooks []Hook) []Hook {
	normalized := make([]Hook, 0, len(hooks))
	for _, hook := range hooks {
		hook.Phase = normalizeHookPhase(hook.Phase)
		if hook.Matcher == nil {
			hook.Matcher = HookMatcherFunc(func(req *HookRequest) bool { return true })
		}
		if hook.Before == nil && hook.After == nil {
			continue
		}
		normalized = append(normalized, hook)
	}
	return normalized
}

func hookMatches(hook Hook, req *HookRequest) bool {
	if req == nil {
		return false
	}
	if hook.Matcher == nil {
		return true
	}
	return hook.Matcher.Match(req)
}

func matchesOpPath(req *HookRequest, ops []HookOp, pathPattern string) bool {
	if req == nil {
		return false
	}
	if len(ops) > 0 {
		matched := false
		for _, op := range ops {
			if op == req.Op {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if pathPattern == "" {
		return true
	}
	matched, err := filepath.Match(pathPattern, req.Path)
	if err != nil {
		return false
	}
	return matched
}

func normalizeHookPhase(phase HookPhase) HookPhase {
	switch strings.ToLower(string(phase)) {
	case string(HookPhaseAfter):
		return HookPhaseAfter
	default:
		return HookPhaseBefore
	}
}

func normalizeHookAction(action HookAction) HookAction {
	switch strings.ToLower(string(action)) {
	case string(HookActionBlock):
		return HookActionBlock
	case string(HookActionMutateWrite):
		return HookActionMutateWrite
	case string(HookActionExecAfter):
		return HookActionExecAfter
	default:
		return HookActionAllow
	}
}
