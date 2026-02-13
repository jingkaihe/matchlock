package state

import "errors"

var (
	ErrNoAvailableSubnets    = errors.New("no available subnets")
	ErrSaveSubnetAllocation  = errors.New("failed to save subnet allocation")
)
