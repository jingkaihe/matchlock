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

	ErrInvalidVolumeFormat = errors.New("expected format host:guest or host:guest:ro")
	ErrResolvePath         = errors.New("failed to resolve path")
	ErrHostPathNotExist    = errors.New("host path does not exist")
	ErrUnknownMountOption  = errors.New("unknown option")
	ErrGuestPathNotAbs     = errors.New("guest path must be absolute")
	ErrGuestPathOutside    = errors.New("guest path must be within workspace")

	ErrEnvNameEmpty   = errors.New("environment variable name cannot be empty")
	ErrEnvNameInvalid = errors.New("environment variable name is invalid")
	ErrEnvVarNotSet   = errors.New("environment variable is not set")
	ErrReadEnvFile    = errors.New("read env file")
	ErrEnvFileLine    = errors.New("parse env file line")

	ErrPortForwardSpecFormat = errors.New("invalid port-forward spec format")
	ErrPortForwardPort       = errors.New("invalid port")
)
