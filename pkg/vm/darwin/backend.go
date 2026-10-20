//go:build darwin && cgo

package darwin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Code-Hex/vz/v3"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

// consoleFiles owns the host-side ends of the guest's serial console. It is
// only meaningful on the VZ (cgo) path.
type consoleFiles struct {
	stdinNull *os.File
	logFile   *os.File
}

func (b *DarwinBackend) Create(ctx context.Context, config *vm.VMConfig) (vm.Machine, error) {
	// Custom kernel args are used verbatim and bypass every generated
	// matchlock.* argument (including matchlock.swap= and matchlock.ipv6=), so
	// reject a swap disk combined with them before doing any work; otherwise the
	// swap device would be attached but never enabled.
	if err := vm.ValidateSwapDisks(config); err != nil {
		return nil, err
	}

	// The same IPv6 link validation the Linux backends perform before any host
	// resource exists, but only for configs that actually use the IPv6 link:
	// the fields are populated for interception sandboxes only (mirroring the
	// sandbox's needsInterception gate). A plain-NAT or --no-network config
	// keeps booting exactly as before even if it carries stale IPv6 fields.
	if config.UseInterception && !config.NoNetwork {
		if _, _, err := vm.ParseIPv6Link(config); err != nil {
			return nil, errx.Wrap(ErrInvalidIPv6Link, err)
		}
	}

	vcpus, ok := api.VCPUCount(config.CPUs)
	if !ok {
		return nil, errx.With(ErrInvalidCPUCount, ": cpus must be a finite number > 0")
	}
	hostCPUs := runtime.NumCPU()
	if vcpus > hostCPUs {
		return nil, errx.With(ErrInvalidCPUCount, ": cpus must be <= host cpus (%d)", hostCPUs)
	}

	// Verify files exist
	if _, err := os.Stat(config.KernelPath); err != nil {
		return nil, errx.With(ErrKernelNotFound, ": %s: %w", config.KernelPath, err)
	}
	if _, err := os.Stat(config.RootfsPath); err != nil {
		return nil, errx.With(ErrRootfsNotFound, ": %s: %w", config.RootfsPath, err)
	}

	// Copy rootfs to temp file so each VM gets a clean image
	// (VMs write to the rootfs and would corrupt the cached image)
	// If PrebuiltRootfs is set, skip the copy (caller already prepared it)
	tempRootfs := config.PrebuiltRootfs
	if tempRootfs == "" {
		var err error
		tempRootfs, err = CopyRootfsToTemp(config.RootfsPath)
		if err != nil {
			return nil, errx.Wrap(ErrCopyRootfs, err)
		}
	}

	var socketPair *SocketPair
	if config.UseInterception && !config.NoNetwork {
		sp, err := createSocketPair()
		if err != nil {
			os.Remove(tempRootfs)
			return nil, errx.Wrap(ErrSocketPair, err)
		}
		socketPair = sp
	}
	closeSocketPair := func() {
		if socketPair != nil {
			socketPair.Close()
		}
	}

	kernelArgs := b.buildKernelArgs(config)

	bootLoaderOpts := []vz.LinuxBootLoaderOption{
		vz.WithCommandLine(kernelArgs),
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		bootLoaderOpts...,
	)
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrBootLoader, err)
	}

	vzConfig, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		uint(vcpus),
		uint64(config.MemoryMB)*1024*1024,
	)
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrVMConfig, err)
	}

	configWithRootfs := *config
	configWithRootfs.RootfsPath = tempRootfs
	if err := b.configureStorage(vzConfig, &configWithRootfs); err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrStorageConfig, err)
	}

	if err := b.configureNetwork(vzConfig, socketPair, config.UseInterception, config.NoNetwork); err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrNetworkConfig, err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrVsockConfig, err)
	}
	vzConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	// Add entropy device for virtio random
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrEntropyConfig, err)
	}
	vzConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	console, err := b.configureConsole(vzConfig, config)
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrConsoleConfig, err)
	}
	releaseConsole := true
	defer func() {
		if !releaseConsole {
			return
		}
		if console.stdinNull != nil {
			_ = console.stdinNull.Close()
			console.stdinNull = nil
		}
		if console.logFile != nil {
			_ = console.logFile.Close()
			console.logFile = nil
		}
	}()

	validated, err := vzConfig.Validate()
	if err != nil || !validated {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.With(ErrVMConfigInvalid, ": validated=%v: %w", validated, err)
	}

	vzVM, err := vz.NewVirtualMachine(vzConfig)
	if err != nil {
		os.Remove(tempRootfs)
		closeSocketPair()
		return nil, errx.Wrap(ErrVMCreate, err)
	}
	releaseConsole = false

	return &DarwinMachine{
		id:          config.ID,
		config:      config,
		vm:          vzVM,
		socketPair:  socketPair,
		tempRootfs:  tempRootfs,
		consoleRead: console.stdinNull,
		consoleLog:  console.logFile,
	}, nil
}

func (b *DarwinBackend) configureStorage(vzConfig *vz.VirtualMachineConfiguration, config *vm.VMConfig) error {
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		config.RootfsPath,
		false,
		vz.DiskImageCachingModeAutomatic,
		vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		return errx.Wrap(ErrDiskAttachment, err)
	}

	storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return errx.Wrap(ErrStorageConfig, err)
	}

	devices := []vz.StorageDeviceConfiguration{storageConfig}

	if config.OverlayEnabled {
		for _, lowerPath := range config.OverlayLowerPaths {
			lowerAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
				lowerPath,
				true,
				vz.DiskImageCachingModeAutomatic,
				vz.DiskImageSynchronizationModeFsync,
			)
			if err != nil {
				return errx.Wrap(ErrDiskAttachment, err)
			}
			lowerConfig, err := vz.NewVirtioBlockDeviceConfiguration(lowerAttachment)
			if err != nil {
				return errx.Wrap(ErrStorageConfig, err)
			}
			devices = append(devices, lowerConfig)
		}

		upperAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			config.OverlayUpperPath,
			false,
			vz.DiskImageCachingModeAutomatic,
			vz.DiskImageSynchronizationModeFsync,
		)
		if err != nil {
			return errx.Wrap(ErrDiskAttachment, err)
		}
		upperConfig, err := vz.NewVirtioBlockDeviceConfiguration(upperAttachment)
		if err != nil {
			return errx.Wrap(ErrStorageConfig, err)
		}
		devices = append(devices, upperConfig)
	}

	for i, disk := range config.ExtraDisks {
		extraAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			disk.HostPath,
			disk.ReadOnly,
			vz.DiskImageCachingModeAutomatic,
			vz.DiskImageSynchronizationModeFsync,
		)
		if err != nil {
			return errx.With(ErrExtraDiskAttach, ": disk %d: %w", i, err)
		}

		extraConfig, err := vz.NewVirtioBlockDeviceConfiguration(extraAttachment)
		if err != nil {
			return errx.With(ErrExtraDiskConfig, ": disk %d: %w", i, err)
		}

		devices = append(devices, extraConfig)
	}

	vzConfig.SetStorageDevicesVirtualMachineConfiguration(devices)
	return nil
}

func (b *DarwinBackend) configureNetwork(vzConfig *vz.VirtualMachineConfiguration, socketPair *SocketPair, useInterception bool, noNetwork bool) error {
	if noNetwork {
		vzConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{})
		return nil
	}

	var netAttachment vz.NetworkDeviceAttachment
	var err error

	if useInterception {
		if socketPair == nil {
			return ErrNetworkConfig
		}
		// Use socket pair for traffic interception via gVisor stack
		netAttachment, err = vz.NewFileHandleNetworkDeviceAttachment(socketPair.GuestFile())
		if err != nil {
			return errx.Wrap(ErrFileHandleAttach, err)
		}
	} else {
		// Use NAT for simple networking without interception
		netAttachment, err = vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return errx.Wrap(ErrNATAttach, err)
		}
	}

	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(netAttachment)
	if err != nil {
		return errx.Wrap(ErrNetworkConfig, err)
	}

	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return errx.Wrap(ErrMACAddress, err)
	}
	netConfig.SetMACAddress(mac)

	vzConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})
	return nil
}

func (b *DarwinBackend) configureConsole(vzConfig *vz.VirtualMachineConfiguration, config *vm.VMConfig) (*consoleFiles, error) {
	// Debug console - kernel output goes to file
	logPath := config.LogPath
	if logPath == "" {
		logPath = "console.log"
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, errx.Wrap(ErrConsoleLog, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0644)
	if err != nil {
		return nil, errx.Wrap(ErrConsoleLog, err)
	}

	nullRead, err := os.Open("/dev/null")
	if err != nil {
		logFile.Close()
		return nil, errx.Wrap(ErrDevNull, err)
	}

	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(nullRead, logFile)
	if err != nil {
		nullRead.Close()
		logFile.Close()
		return nil, errx.Wrap(ErrSerialAttach, err)
	}

	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		nullRead.Close()
		logFile.Close()
		return nil, errx.Wrap(ErrConsoleDevice, err)
	}

	vzConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleConfig})
	return &consoleFiles{
		stdinNull: nullRead,
		logFile:   logFile,
	}, nil
}
