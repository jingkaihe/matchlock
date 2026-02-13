package sandbox

import (
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vfs"
)

func buildVFSHookEngine(config *api.Config) *vfs.HookEngine {
	if config == nil || config.VFS == nil || config.VFS.Interception == nil {
		return nil
	}

	hookCfg := config.VFS.Interception
	if len(hookCfg.Rules) == 0 && !hookCfg.EmitEvents {
		return nil
	}

	rules := make([]vfs.HookRule, 0, len(hookCfg.Rules))
	for _, cfgRule := range hookCfg.Rules {
		action, ok := parseVFSHookAction(cfgRule.Action)
		if !ok {
			continue
		}

		ops := make([]vfs.HookOp, 0, len(cfgRule.Ops))
		for _, opName := range cfgRule.Ops {
			if op, ok := parseVFSHookOp(opName); ok {
				ops = append(ops, op)
			}
		}

		rule := vfs.HookRule{
			Name:        cfgRule.Name,
			Phase:       parseVFSHookPhase(cfgRule.Phase),
			Ops:         ops,
			PathPattern: cfgRule.Path,
			Action:      action,
			MutateWrite: []byte(cfgRule.Data),
			ExecCommand: cfgRule.Command,
		}
		if cfgRule.TimeoutMS > 0 {
			rule.ExecTimeout = time.Duration(cfgRule.TimeoutMS) * time.Millisecond
		}

		rules = append(rules, rule)
	}

	if len(rules) == 0 && !hookCfg.EmitEvents {
		return nil
	}

	return vfs.NewHookEngine(rules, hookCfg.MaxExecDepth)
}

func attachVFSFileEvents(hooks *vfs.HookEngine, events chan api.Event) {
	if hooks == nil || events == nil {
		return
	}

	hooks.SetEventFunc(func(req vfs.HookRequest, result vfs.HookResult) {
		evt := api.Event{
			Type:      "file",
			Timestamp: time.Now().UnixMilli(),
			File: &api.FileEvent{
				Op:   string(req.Op),
				Path: req.Path,
				Size: int64(result.Bytes),
			},
		}
		select {
		case events <- evt:
		default:
		}
	})
}

func parseVFSHookPhase(phase string) vfs.HookPhase {
	if strings.EqualFold(phase, string(vfs.HookPhaseAfter)) {
		return vfs.HookPhaseAfter
	}
	return vfs.HookPhaseBefore
}

func parseVFSHookAction(action string) (vfs.HookAction, bool) {
	switch strings.ToLower(action) {
	case string(vfs.HookActionAllow):
		return vfs.HookActionAllow, true
	case string(vfs.HookActionBlock):
		return vfs.HookActionBlock, true
	case string(vfs.HookActionMutateWrite):
		return vfs.HookActionMutateWrite, true
	case string(vfs.HookActionExecAfter), "exec":
		return vfs.HookActionExecAfter, true
	default:
		return "", false
	}
}

func parseVFSHookOp(op string) (vfs.HookOp, bool) {
	switch strings.ToLower(op) {
	case string(vfs.HookOpStat):
		return vfs.HookOpStat, true
	case string(vfs.HookOpReadDir), "read_dir":
		return vfs.HookOpReadDir, true
	case string(vfs.HookOpOpen):
		return vfs.HookOpOpen, true
	case string(vfs.HookOpCreate):
		return vfs.HookOpCreate, true
	case string(vfs.HookOpMkdir):
		return vfs.HookOpMkdir, true
	case string(vfs.HookOpChmod):
		return vfs.HookOpChmod, true
	case string(vfs.HookOpRemove):
		return vfs.HookOpRemove, true
	case string(vfs.HookOpRemoveAll), "removeall":
		return vfs.HookOpRemoveAll, true
	case string(vfs.HookOpRename):
		return vfs.HookOpRename, true
	case string(vfs.HookOpSymlink):
		return vfs.HookOpSymlink, true
	case string(vfs.HookOpReadlink):
		return vfs.HookOpReadlink, true
	case string(vfs.HookOpRead):
		return vfs.HookOpRead, true
	case string(vfs.HookOpWrite):
		return vfs.HookOpWrite, true
	case string(vfs.HookOpClose):
		return vfs.HookOpClose, true
	case string(vfs.HookOpSync):
		return vfs.HookOpSync, true
	case string(vfs.HookOpTruncate):
		return vfs.HookOpTruncate, true
	default:
		return "", false
	}
}
