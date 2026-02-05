package api

import "errors"

var (
	ErrBlocked        = errors.New("request blocked by policy")
	ErrHostNotAllowed = errors.New("host not in allowlist")
	ErrSecretLeak     = errors.New("secret placeholder sent to unauthorized host")
	ErrVMNotRunning   = errors.New("VM is not running")
	ErrVMNotFound     = errors.New("VM not found")
	ErrTimeout        = errors.New("operation timed out")
	ErrInvalidConfig  = errors.New("invalid configuration")
)
