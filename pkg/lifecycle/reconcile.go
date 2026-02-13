package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/state"
)

type ReconcileReport struct {
	VMID    string
	Cleaned []string
	Failed  map[string]string
	Skipped string
}

func (r *ReconcileReport) addCleaned(item string) {
	r.Cleaned = append(r.Cleaned, item)
}

func (r *ReconcileReport) addFailed(item string, err error) {
	if r.Failed == nil {
		r.Failed = make(map[string]string)
	}
	r.Failed[item] = err.Error()
}

type Reconciler struct {
	stateMgr    *state.Manager
	subnetAlloc *state.SubnetAllocator
}

func NewReconciler() *Reconciler {
	return &Reconciler{
		stateMgr:    state.NewManager(),
		subnetAlloc: state.NewSubnetAllocator(),
	}
}

func NewReconcilerWithManagers(stateMgr *state.Manager, subnetAlloc *state.SubnetAllocator) *Reconciler {
	return &Reconciler{
		stateMgr:    stateMgr,
		subnetAlloc: subnetAlloc,
	}
}

func (r *Reconciler) ReconcileAll(forceRunning bool) ([]ReconcileReport, error) {
	states, err := r.stateMgr.List()
	if err != nil {
		return nil, err
	}

	reports := make([]ReconcileReport, 0, len(states))
	var errs []error
	for _, s := range states {
		report, reconcileErr := r.ReconcileVM(s.ID, forceRunning)
		reports = append(reports, report)
		if reconcileErr != nil {
			errs = append(errs, errx.With(reconcileErr, ": %s", s.ID))
		}
	}

	sort.Slice(reports, func(i, j int) bool { return reports[i].VMID < reports[j].VMID })
	if len(errs) > 0 {
		return reports, errors.Join(errs...)
	}
	return reports, nil
}

func (r *Reconciler) ReconcileVM(vmID string, forceRunning bool) (ReconcileReport, error) {
	report := ReconcileReport{VMID: vmID}
	vmState, err := r.stateMgr.Get(vmID)
	if err != nil {
		report.addFailed("state", err)
		return report, err
	}

	if vmState.Status == "running" && processRunning(vmState.PID) && !forceRunning {
		report.Skipped = ErrVMRunning.Error()
		return report, errx.With(ErrVMRunning, " %s", vmID)
	}

	vmDir := r.stateMgr.Dir(vmID)
	store := NewStore(vmDir)
	if !store.Exists() {
		if err := store.Init(vmID, "unknown", vmDir); err == nil {
			_ = store.SetResource(func(res *Resources) {
				res.RootfsPath = filepath.Join(vmDir, "rootfs.ext4")
			})
		}
	}
	rec, err := store.Load()
	if err != nil {
		report.addFailed("load_record", err)
		return report, err
	}
	if rec.VMID == "" {
		rec.VMID = vmID
	}

	_ = store.SetPhase(PhaseCleaning)
	_ = store.SetLastError(nil)

	stepErr := r.reconcileSubnet(vmID, store, &report)
	rootfsErr := r.reconcileRootfs(rec, store, &report)
	platformErr := r.reconcilePlatform(rec, store, &report)

	var errs []error
	for _, e := range []error{stepErr, rootfsErr, platformErr} {
		if e != nil {
			errs = append(errs, e)
		}
	}

	if len(errs) > 0 {
		joined := errors.Join(errs...)
		_ = store.SetLastError(joined)
		_ = store.SetPhase(PhaseCleanupFailed)
		return report, joined
	}

	_ = store.SetLastError(nil)
	_ = store.SetPhase(PhaseCleaned)
	return report, nil
}

func (r *Reconciler) reconcileSubnet(vmID string, store *Store, report *ReconcileReport) error {
	err := r.subnetAlloc.Release(vmID)
	_ = store.MarkCleanup("subnet_release", err)
	if err != nil {
		report.addFailed("subnet_release", err)
		return errx.With(err, ": %s", vmID)
	}
	report.addCleaned("subnet_release")
	return nil
}

func (r *Reconciler) reconcileRootfs(rec *Record, store *Store, report *ReconcileReport) error {
	rootfsPath := rec.Resources.RootfsPath
	if rootfsPath == "" {
		rootfsPath = filepath.Join(r.stateMgr.Dir(rec.VMID), "rootfs.ext4")
	}
	if rootfsPath == "" {
		_ = store.MarkCleanup("rootfs_remove", nil)
		return nil
	}
	err := os.Remove(rootfsPath)
	if err != nil && !os.IsNotExist(err) {
		_ = store.MarkCleanup("rootfs_remove", err)
		report.addFailed("rootfs_remove", err)
		return errx.With(err, ": %s", rootfsPath)
	}
	_ = store.MarkCleanup("rootfs_remove", nil)
	report.addCleaned("rootfs_remove")
	return nil
}

func processRunning(pid int) bool {
	if pid == 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func tapNameCandidates(vmID, fromRecord string) []string {
	candidates := make([]string, 0, 3)
	add := func(v string) {
		if v == "" {
			return
		}
		for _, existing := range candidates {
			if existing == v {
				return
			}
		}
		candidates = append(candidates, v)
	}

	add(fromRecord)
	idSuffix := strings.TrimPrefix(vmID, "vm-")
	if len(idSuffix) >= 8 {
		add("fc-" + idSuffix[:8])
	} else if idSuffix != "" {
		add("fc-" + idSuffix)
	}
	if len(vmID) >= 8 {
		add(fmt.Sprintf("fc-%s", vmID[:8]))
	}
	return candidates
}
