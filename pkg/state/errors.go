package state

import "errors"

var (
	ErrStateDBInit  = errors.New("initialize state database")
	ErrRegisterVM   = errors.New("register VM")
	ErrUnregisterVM = errors.New("unregister VM")
	ErrListVMs      = errors.New("list VMs")
	ErrGetVM        = errors.New("get VM")
	ErrKillVM       = errors.New("kill VM")
	ErrRemoveVM     = errors.New("remove VM")
	ErrReadLog      = errors.New("read VM log")
	ErrWriteLog     = errors.New("write VM log")

	ErrNoAvailableSubnets   = errors.New("no available subnets")
	ErrSaveSubnetAllocation = errors.New("failed to save subnet allocation")
)
