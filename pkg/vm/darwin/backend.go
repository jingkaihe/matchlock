//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

const (
	VsockPortExec  = 5000
	VsockPortVFS   = 5001
	VsockPortReady = 5002
)

type DarwinBackend struct{}

func NewDarwinBackend() *DarwinBackend {
	return &DarwinBackend{}
}

func (b *DarwinBackend) Name() string {
	return "virtualization.framework"
}

func (b *DarwinBackend) Create(ctx context.Context, config *vm.VMConfig) (vm.Machine, error) {
	// Verify files exist
	if _, err := os.Stat(config.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel not found: %s: %w", config.KernelPath, err)
	}
	if _, err := os.Stat(config.RootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs not found: %s: %w", config.RootfsPath, err)
	}

	// Copy rootfs to temp file so each VM gets a clean image
	// (VMs write to the rootfs and would corrupt the cached image)
	// If PrebuiltRootfs is set, skip the copy (caller already prepared it)
	tempRootfs := config.PrebuiltRootfs
	if tempRootfs == "" {
		var err error
		tempRootfs, err = CopyRootfsToTemp(config.RootfsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to copy rootfs: %w", err)
		}
	}

	socketPair, err := createSocketPair()
	if err != nil {
		os.Remove(tempRootfs)
		return nil, fmt.Errorf("failed to create socket pair: %w", err)
	}

	kernelArgs := b.buildKernelArgs(config)

	bootLoaderOpts := []vz.LinuxBootLoaderOption{
		vz.WithCommandLine(kernelArgs),
	}
	if config.InitramfsPath != "" {
		if _, err := os.Stat(config.InitramfsPath); err != nil {
			os.Remove(tempRootfs)
			socketPair.Close()
			return nil, fmt.Errorf("initramfs not found: %s: %w", config.InitramfsPath, err)
		}
		bootLoaderOpts = append(bootLoaderOpts, vz.WithInitrd(config.InitramfsPath))
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		bootLoaderOpts...,
	)
	if err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to create boot loader: %w", err)
	}

	vzConfig, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		uint(config.CPUs),
		uint64(config.MemoryMB)*1024*1024,
	)
	if err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to create VM configuration: %w", err)
	}

	configWithRootfs := *config
	configWithRootfs.RootfsPath = tempRootfs
	if err := b.configureStorage(vzConfig, &configWithRootfs); err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure storage: %w", err)
	}

	if err := b.configureNetwork(vzConfig, socketPair, config.UseInterception); err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure network: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to create vsock config: %w", err)
	}
	vzConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	// Add entropy device for virtio random
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to create entropy config: %w", err)
	}
	vzConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	if err := b.configureConsole(vzConfig, config); err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure console: %w", err)
	}

	validated, err := vzConfig.Validate()
	if err != nil || !validated {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("VM configuration validation failed: validated=%v, err=%w", validated, err)
	}

	vzVM, err := vz.NewVirtualMachine(vzConfig)
	if err != nil {
		os.Remove(tempRootfs)
		socketPair.Close()
		return nil, fmt.Errorf("failed to create virtual machine: %w", err)
	}

	return &DarwinMachine{
		id:          config.ID,
		config:      config,
		vm:          vzVM,
		socketPair:  socketPair,
		tempRootfs:  tempRootfs,
	}, nil
}

func (b *DarwinBackend) buildKernelArgs(config *vm.VMConfig) string {
	if config.KernelArgs != "" {
		return config.KernelArgs
	}

	workspace := config.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}

	// Root device is /dev/vda (first virtio block device)
	privilegedArg := ""
	if config.Privileged {
		privilegedArg = " matchlock.privileged=1"
	}

	diskArgs := ""
	for i, disk := range config.ExtraDisks {
		dev := string(rune('b' + i))
		diskArgs += fmt.Sprintf(" matchlock.disk.vd%s=%s", dev, disk.GuestMount)
	}

	if config.UseInterception {
		guestIP := config.GuestIP
		if guestIP == "" {
			guestIP = "192.168.100.2"
		}
		gatewayIP := config.GatewayIP
		if gatewayIP == "" {
			gatewayIP = "192.168.100.1"
		}
		return fmt.Sprintf(
			"console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 ip=%s::%s:255.255.255.0::eth0:off%s matchlock.workspace=%s matchlock.dns=%s%s%s",
			guestIP, gatewayIP, vm.KernelIPDNSSuffix(config.DNSServers), workspace, vm.KernelDNSParam(config.DNSServers), privilegedArg, diskArgs,
		)
	}

	return fmt.Sprintf(
		"console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 ip=dhcp matchlock.workspace=%s matchlock.dns=%s%s%s",
		workspace, vm.KernelDNSParam(config.DNSServers), privilegedArg, diskArgs,
	)
}

func (b *DarwinBackend) configureStorage(vzConfig *vz.VirtualMachineConfiguration, config *vm.VMConfig) error {
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		config.RootfsPath,
		false,
		vz.DiskImageCachingModeAutomatic,
		vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		return fmt.Errorf("failed to create disk attachment: %w", err)
	}

	storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return fmt.Errorf("failed to create storage config: %w", err)
	}

	devices := []vz.StorageDeviceConfiguration{storageConfig}

	for i, disk := range config.ExtraDisks {
		extraAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			disk.HostPath,
			disk.ReadOnly,
			vz.DiskImageCachingModeAutomatic,
			vz.DiskImageSynchronizationModeFsync,
		)
		if err != nil {
			return fmt.Errorf("failed to create extra disk %d attachment: %w", i, err)
		}

		extraConfig, err := vz.NewVirtioBlockDeviceConfiguration(extraAttachment)
		if err != nil {
			return fmt.Errorf("failed to create extra disk %d config: %w", i, err)
		}

		devices = append(devices, extraConfig)
	}

	vzConfig.SetStorageDevicesVirtualMachineConfiguration(devices)
	return nil
}

// CopyRootfsToTemp copies the rootfs image to a temp file so each VM gets a clean copy
func CopyRootfsToTemp(srcPath string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	// Create temp file in same directory to ensure same filesystem (for efficient copy)
	dir := filepath.Dir(srcPath)
	dst, err := os.CreateTemp(dir, "matchlock-rootfs-*.ext4")
	if err != nil {
		// Fall back to system temp if same dir fails
		dst, err = os.CreateTemp("", "matchlock-rootfs-*.ext4")
		if err != nil {
			return "", err
		}
	}
	dstPath := dst.Name()

	_, err = io.Copy(dst, src)
	if err != nil {
		dst.Close()
		os.Remove(dstPath)
		return "", err
	}

	if err := dst.Close(); err != nil {
		os.Remove(dstPath)
		return "", err
	}

	return dstPath, nil
}

func (b *DarwinBackend) configureNetwork(vzConfig *vz.VirtualMachineConfiguration, socketPair *SocketPair, useInterception bool) error {
	var netAttachment vz.NetworkDeviceAttachment
	var err error

	if useInterception {
		// Use socket pair for traffic interception via gVisor stack
		netAttachment, err = vz.NewFileHandleNetworkDeviceAttachment(socketPair.GuestFile())
		if err != nil {
			return fmt.Errorf("failed to create file handle network attachment: %w", err)
		}
	} else {
		// Use NAT for simple networking without interception
		netAttachment, err = vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return fmt.Errorf("failed to create NAT network attachment: %w", err)
		}
	}

	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(netAttachment)
	if err != nil {
		return fmt.Errorf("failed to create network config: %w", err)
	}

	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return fmt.Errorf("failed to generate MAC address: %w", err)
	}
	netConfig.SetMACAddress(mac)

	vzConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})
	return nil
}

func (b *DarwinBackend) configureConsole(vzConfig *vz.VirtualMachineConfiguration, config *vm.VMConfig) error {
	// Debug console - kernel output goes to file
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".cache", "matchlock", "console.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create console log: %w", err)
	}

	nullRead, err := os.Open("/dev/null")
	if err != nil {
		logFile.Close()
		return fmt.Errorf("failed to open /dev/null for reading: %w", err)
	}

	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(nullRead, logFile)
	if err != nil {
		nullRead.Close()
		logFile.Close()
		return fmt.Errorf("failed to create serial attachment: %w", err)
	}

	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("failed to create console config: %w", err)
	}

	vzConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleConfig})
	return nil
}
