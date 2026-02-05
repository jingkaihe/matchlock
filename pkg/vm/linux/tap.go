package linux

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

const (
	tunDevice = "/dev/net/tun"
	ifnameLen = 16
)

type ifreq struct {
	name  [ifnameLen]byte
	flags uint16
	_     [22]byte
}

func CreateTAP(name string) (int, error) {
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open %s: %w", tunDevice, err)
	}

	var ifr ifreq
	copy(ifr.name[:], name)
	ifr.flags = syscall.IFF_TAP | syscall.IFF_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		syscall.Close(fd)
		return 0, fmt.Errorf("TUNSETIFF failed: %w", errno)
	}

	return fd, nil
}

func ConfigureInterface(name, cidr string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", name, err)
	}

	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid CIDR %s: %w", cidr, err)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}
	defer syscall.Close(fd)

	var ifr struct {
		name [ifnameLen]byte
		addr syscall.RawSockaddrInet4
		_    [8]byte
	}
	copy(ifr.name[:], name)
	ifr.addr.Family = syscall.AF_INET
	copy(ifr.addr.Addr[:], ip.To4())

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFADDR, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return fmt.Errorf("SIOCSIFADDR failed: %w", errno)
	}

	var maskReq struct {
		name [ifnameLen]byte
		addr syscall.RawSockaddrInet4
		_    [8]byte
	}
	copy(maskReq.name[:], name)
	maskReq.addr.Family = syscall.AF_INET
	copy(maskReq.addr.Addr[:], ipNet.Mask)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFNETMASK, uintptr(unsafe.Pointer(&maskReq))); errno != 0 {
		return fmt.Errorf("SIOCSIFNETMASK failed: %w", errno)
	}

	var flagReq struct {
		name  [ifnameLen]byte
		flags int16
		_     [22]byte
	}
	copy(flagReq.name[:], name)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&flagReq))); errno != 0 {
		return fmt.Errorf("SIOCGIFFLAGS failed: %w", errno)
	}

	flagReq.flags |= syscall.IFF_UP | syscall.IFF_RUNNING

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&flagReq))); errno != 0 {
		return fmt.Errorf("SIOCSIFFLAGS failed: %w", errno)
	}

	_ = iface
	return nil
}

func DeleteInterface(name string) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr ifreq
	copy(ifr.name[:], name)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.SIOCSIFFLAGS), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}

	return nil
}

func SetMTU(name string, mtu int) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr struct {
		name [ifnameLen]byte
		mtu  int32
		_    [20]byte
	}
	copy(ifr.name[:], name)
	ifr.mtu = int32(mtu)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFMTU, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}

	return nil
}

func GenerateMAC(vmID string) string {
	h := uint32(0)
	for _, c := range vmID {
		h = h*31 + uint32(c)
	}
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X",
		byte(h>>24), byte(h>>16), byte(h>>8), byte(h))
}

func SetupIPForwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

func SetupNAT(tapName, extIface string) error {
	return nil
}
