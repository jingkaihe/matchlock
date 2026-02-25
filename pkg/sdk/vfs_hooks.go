package sdk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

func compileVFSHooks(cfg *VFSInterceptionConfig) (*VFSInterceptionConfig, []compiledVFSHook, []compiledVFSMutateHook, []compiledVFSActionHook, error) {
	if cfg == nil {
		return nil, nil, nil, nil, nil
	}

	wire := &VFSInterceptionConfig{
		EmitEvents: cfg.EmitEvents,
	}
	local := make([]compiledVFSHook, 0, len(cfg.Rules))
	localMutate := make([]compiledVFSMutateHook, 0, len(cfg.Rules))
	localAction := make([]compiledVFSActionHook, 0, len(cfg.Rules))
	wire.Rules = make([]VFSHookRule, 0, len(cfg.Rules))

	for _, rule := range cfg.Rules {
		callbackCount := 0
		if rule.Hook != nil {
			callbackCount++
		}
		if rule.DangerousHook != nil {
			callbackCount++
		}
		if rule.MutateHook != nil {
			callbackCount++
		}
		if rule.ActionHook != nil {
			callbackCount++
		}
		if callbackCount > 1 {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q cannot set more than one callback hook", rule.Name)
		}

		if rule.Hook == nil && rule.DangerousHook == nil && rule.MutateHook == nil && rule.ActionHook == nil {
			action := strings.ToLower(strings.TrimSpace(string(rule.Action)))
			switch action {
			case "mutate_write":
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate_write requires MutateHook callback", rule.Name)
			}
			wire.Rules = append(wire.Rules, rule)
			continue
		}

		if rule.Hook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q callback hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if !strings.EqualFold(rule.Phase, VFSHookPhaseAfter) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q must use phase=after", rule.Name)
			}

			compiled := compiledVFSHook{
				name: rule.Name,
				path: rule.Path,
				callback: func(ctx context.Context, _ *Client, event VFSHookEvent) error {
					return rule.Hook(ctx, event)
				},
			}
			if rule.TimeoutMS > 0 {
				compiled.timeout = time.Duration(rule.TimeoutMS) * time.Millisecond
			}
			if len(rule.Ops) > 0 {
				compiled.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiled.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			local = append(local, compiled)
			continue
		}

		if rule.DangerousHook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q dangerous hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if !strings.EqualFold(rule.Phase, VFSHookPhaseAfter) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q dangerous hooks must use phase=after", rule.Name)
			}

			compiled := compiledVFSHook{
				name:      rule.Name,
				path:      rule.Path,
				timeout:   0,
				dangerous: true,
				callback: func(ctx context.Context, client *Client, event VFSHookEvent) error {
					return rule.DangerousHook(ctx, client, event)
				},
			}
			if rule.TimeoutMS > 0 {
				compiled.timeout = time.Duration(rule.TimeoutMS) * time.Millisecond
			}
			if len(rule.Ops) > 0 {
				compiled.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiled.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			local = append(local, compiled)
			continue
		}

		if rule.ActionHook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q action hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if rule.Phase != "" && !strings.EqualFold(rule.Phase, VFSHookPhaseBefore) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q action hook must use phase=before", rule.Name)
			}
			compiledAction := compiledVFSActionHook{
				name:     rule.Name,
				path:     rule.Path,
				callback: rule.ActionHook,
			}
			if len(rule.Ops) > 0 {
				compiledAction.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiledAction.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			localAction = append(localAction, compiledAction)
			continue
		}

		if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate hooks cannot set action=%q", rule.Name, rule.Action)
		}
		if rule.Phase != "" && !strings.EqualFold(rule.Phase, VFSHookPhaseBefore) {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate hook must use phase=before", rule.Name)
		}

		compiledMutate := compiledVFSMutateHook{
			name:     rule.Name,
			path:     rule.Path,
			callback: rule.MutateHook,
		}
		if len(rule.Ops) > 0 {
			compiledMutate.ops = make(map[string]struct{}, len(rule.Ops))
			for _, op := range rule.Ops {
				if op == "" {
					continue
				}
				compiledMutate.ops[strings.ToLower(op)] = struct{}{}
			}
		}
		localMutate = append(localMutate, compiledMutate)
	}

	if len(local) > 0 {
		wire.EmitEvents = true
	}

	if len(wire.Rules) == 0 && !wire.EmitEvents {
		wire = nil
	}

	return wire, local, localMutate, localAction, nil
}

func (c *Client) setVFSHooks(hooks []compiledVFSHook, mutateHooks []compiledVFSMutateHook, actionHooks []compiledVFSActionHook) {
	c.vfsHookMu.Lock()
	c.vfsHooks = hooks
	c.vfsMutateHooks = mutateHooks
	c.vfsActionHooks = actionHooks
	c.vfsHookActive.Store(false)
	c.vfsHookMu.Unlock()
}

func (c *Client) handleVFSFileEvent(op, path string, size int64, mode uint32, uid, gid int) {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSHook(nil), c.vfsHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return
	}
	event := VFSHookEvent{
		Op:   VFSHookOp(op),
		Path: path,
		Size: size,
		Mode: mode,
		UID:  uid,
		GID:  gid,
	}

	opLower := strings.ToLower(op)
	safeHooks := make([]compiledVFSHook, 0, len(hooks))
	for _, hook := range hooks {
		if !matchesVFSHook(hook, opLower, path) {
			continue
		}
		if hook.dangerous {
			go c.runSingleVFSHook(hook, event)
			continue
		}
		safeHooks = append(safeHooks, hook)
	}

	if len(safeHooks) == 0 {
		return
	}
	if c.vfsHookActive.Load() {
		return
	}

	go c.runVFSSafeHooksForEvent(safeHooks, event)
}

func (c *Client) runVFSSafeHooksForEvent(hooks []compiledVFSHook, event VFSHookEvent) {
	if !c.vfsHookActive.CompareAndSwap(false, true) {
		return
	}
	defer c.vfsHookActive.Store(false)

	for _, hook := range hooks {
		c.runSingleVFSHook(hook, event)
	}
}

func (c *Client) runSingleVFSHook(hook compiledVFSHook, event VFSHookEvent) {
	ctx := context.Background()
	cancel := func() {}
	if hook.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, hook.timeout)
	}
	_ = hook.callback(ctx, c, event)
	cancel()
}

func matchesVFSHook(hook compiledVFSHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func matchesVFSMutateHook(hook compiledVFSMutateHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func matchesVFSActionHook(hook compiledVFSActionHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func (c *Client) applyLocalActionHooks(ctx context.Context, op VFSHookOp, path string, size int, mode uint32) error {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSActionHook(nil), c.vfsActionHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return nil
	}

	req := VFSActionRequest{
		Op:   op,
		Path: path,
		Size: size,
		Mode: mode,
		UID:  os.Geteuid(),
		GID:  os.Getegid(),
	}

	for _, hook := range hooks {
		if !matchesVFSActionHook(hook, string(op), path) {
			continue
		}

		decision := VFSHookAction(strings.ToLower(strings.TrimSpace(string(hook.callback(ctx, req)))))
		switch decision {
		case "", VFSHookActionAllow:
			continue
		case VFSHookActionBlock:
			return errx.With(ErrVFSHookBlocked, " op=%s path=%s hook=%q", op, path, hook.name)
		default:
			return errx.With(ErrInvalidVFSHook, " %q returned invalid action decision %q", hook.name, decision)
		}
	}

	return nil
}

func (c *Client) applyLocalWriteMutations(ctx context.Context, path string, content []byte, mode uint32) ([]byte, error) {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSMutateHook(nil), c.vfsMutateHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return content, nil
	}

	current := content
	for _, hook := range hooks {
		if !matchesVFSMutateHook(hook, string(VFSHookOpWrite), path) {
			continue
		}
		req := VFSMutateRequest{
			Path: path,
			Size: len(current),
			Mode: mode,
			UID:  os.Geteuid(),
			GID:  os.Getegid(),
		}
		mutated, err := hook.callback(ctx, req)
		if err != nil {
			return nil, err
		}
		if mutated != nil {
			current = mutated
		}
	}

	return current, nil
}

// PortForward applies one or more [LOCAL_PORT:]REMOTE_PORT mappings with the
// default bind address (127.0.0.1).
