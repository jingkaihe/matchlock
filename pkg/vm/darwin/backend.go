//go:build darwin

package darwin

import (
	"context"
	"fmt"

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
	socketPair, err := createSocketPair()
	if err != nil {
		return nil, fmt.Errorf("failed to create socket pair: %w", err)
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		vz.WithCommandLine(b.buildKernelArgs(config)),
	)
	if err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to create boot loader: %w", err)
	}

	vzConfig, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		uint(config.CPUs),
		uint64(config.MemoryMB)*1024*1024,
	)
	if err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to create VM configuration: %w", err)
	}

	if err := b.configureStorage(vzConfig, config); err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure storage: %w", err)
	}

	if err := b.configureNetwork(vzConfig, socketPair); err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure network: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to create vsock config: %w", err)
	}
	vzConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	if err := b.configureConsole(vzConfig, config); err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to configure console: %w", err)
	}

	validated, err := vzConfig.Validate()
	if err != nil || !validated {
		socketPair.Close()
		return nil, fmt.Errorf("VM configuration validation failed: %w", err)
	}

	vzVM, err := vz.NewVirtualMachine(vzConfig)
	if err != nil {
		socketPair.Close()
		return nil, fmt.Errorf("failed to create virtual machine: %w", err)
	}

	return &DarwinMachine{
		id:         config.ID,
		config:     config,
		vm:         vzVM,
		socketPair: socketPair,
	}, nil
}

func (b *DarwinBackend) buildKernelArgs(config *vm.VMConfig) string {
	if config.KernelArgs != "" {
		return config.KernelArgs
	}

	guestIP := config.GuestIP
	if guestIP == "" {
		guestIP = "192.168.100.2"
	}
	gatewayIP := config.GatewayIP
	if gatewayIP == "" {
		gatewayIP = "192.168.100.1"
	}
	workspace := config.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}

	return fmt.Sprintf(
		"console=hvc0 reboot=k panic=1 ip=%s::%s:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4 matchlock.workspace=%s",
		guestIP, gatewayIP, workspace,
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

	vzConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{storageConfig})
	return nil
}

func (b *DarwinBackend) configureNetwork(vzConfig *vz.VirtualMachineConfiguration, socketPair *SocketPair) error {
	netAttachment, err := vz.NewFileHandleNetworkDeviceAttachment(socketPair.GuestFile())
	if err != nil {
		return fmt.Errorf("failed to create network attachment: %w", err)
	}

	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(netAttachment)
	if err != nil {
		return fmt.Errorf("failed to create network config: %w", err)
	}

	vzConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})
	return nil
}

func (b *DarwinBackend) configureConsole(vzConfig *vz.VirtualMachineConfiguration, config *vm.VMConfig) error {
	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create serial attachment: %w", err)
	}

	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("failed to create console config: %w", err)
	}

	vzConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleConfig})
	return nil
}
