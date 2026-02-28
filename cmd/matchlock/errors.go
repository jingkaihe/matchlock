package main

import "errors"

// Build errors
var (
	ErrGetHomeDir          = errors.New("get home dir")
	ErrCreateCacheDir      = errors.New("create cache dir")
	ErrCreateCacheImage    = errors.New("create cache image")
	ErrTruncateCacheImage  = errors.New("truncate cache image")
	ErrOpenLockFile        = errors.New("open lock file")
	ErrAcquireLock         = errors.New("acquire lock")
	ErrAutoDetectMemory    = errors.New("cannot auto-detect system memory")
	ErrResolveContextDir   = errors.New("resolve context dir")
	ErrResolveDockerfile   = errors.New("resolve Dockerfile")
	ErrBuildBuildKitRootfs = errors.New("building BuildKit rootfs")
	ErrCreateWorkspaceDir  = errors.New("create workspace temp dir")
	ErrCreateOutputDir     = errors.New("create output temp dir")
	ErrResolveCachePath    = errors.New("resolve build cache path")
	ErrLockBuildCache      = errors.New("lock build cache")
	ErrPrepareBuildCache   = errors.New("prepare build cache")
	ErrCreateBuildSandbox  = errors.New("creating BuildKit sandbox")
	ErrStartBuildSandbox   = errors.New("starting BuildKit sandbox")
	ErrWriteBuildScript    = errors.New("write build script")
	ErrBuildKitBuild       = errors.New("BuildKit build")
	ErrOpenImageTarball    = errors.New("open built image tarball")
	ErrImportImage         = errors.New("import built image")
)

// Exec errors
var (
	ErrVMNotFound      = errors.New("VM not found")
	ErrExecFailed      = errors.New("exec failed")
	ErrPipeExecFailed  = errors.New("pipe exec failed")
	ErrSetRawMode      = errors.New("setting raw mode")
	ErrInteractiveExec = errors.New("interactive exec failed")
)

// Pull errors
var (
	ErrSaveTag = errors.New("saving tag")
)

// RPC errors
var (
	ErrBuildRootfs = errors.New("failed to build rootfs")
)

// Run errors
var (
	ErrBuildingRootfs         = errors.New("building rootfs")
	ErrInvalidVolume          = errors.New("invalid volume mount")
	ErrInvalidDiskMount       = errors.New("invalid disk mount")
	ErrInvalidVolumeName      = errors.New("invalid volume name")
	ErrResolveVolumeDir       = errors.New("resolve volume directory")
	ErrCreateVolumeDir        = errors.New("create volume directory")
	ErrCreateVolume           = errors.New("create named volume")
	ErrListVolumes            = errors.New("list named volumes")
	ErrRemoveVolume           = errors.New("remove named volume")
	ErrCopyVolume             = errors.New("copy named volume")
	ErrVolumeExists           = errors.New("named volume already exists")
	ErrVolumeNotFound         = errors.New("named volume not found")
	ErrInvalidSecret          = errors.New("invalid secret")
	ErrInvalidAddHost         = errors.New("invalid add-host mapping")
	ErrInvalidEnv             = errors.New("invalid environment variable")
	ErrInvalidPortForward     = errors.New("invalid port-forward specification")
	ErrInvalidPortForwardAddr = errors.New("invalid port-forward bind address")
	ErrPortForwardListen      = errors.New("start port-forward listener")
	ErrInvalidAllowListHost   = errors.New("invalid allow-list host")
	ErrAllowListUpdate        = errors.New("update allow-list")
	ErrResolveExecutable      = errors.New("resolve executable")
	ErrPrepareDetachedIO      = errors.New("prepare detached stdio")
	ErrStartDetachedRun       = errors.New("start detached run")
	ErrFindDetachedVM         = errors.New("find detached VM")
	ErrCreateSandbox          = errors.New("creating sandbox")
	ErrStartSandbox           = errors.New("starting sandbox")
	ErrCloseSandbox           = errors.New("closing sandbox")
	ErrRemoveSandbox          = errors.New("removing sandbox")
	ErrExecCommand            = errors.New("executing command")
)

// Setup errors (Linux)
var (
	ErrDetermineUser  = errors.New("could not determine user")
	ErrDownloadFailed = errors.New("download failed")
	ErrGzipReader     = errors.New("gzip reader")
	ErrTarReader      = errors.New("tar reader")
	ErrCreateFile     = errors.New("create file")
	ErrWriteFile      = errors.New("write file")
	ErrCreateNetdev   = errors.New("create netdev group")
	ErrAddToNetdev    = errors.New("add user to netdev group")
	ErrChownTun       = errors.New("chown /dev/net/tun")
	ErrWriteSysctl    = errors.New("write sysctl config")
	ErrNfTablesModule = errors.New("nf_tables module not available")
)

// Sysinfo errors
var (
	ErrSysctlMemsize = errors.New("sysctl hw.memsize")
	ErrSysinfo       = errors.New("sysinfo")
)
