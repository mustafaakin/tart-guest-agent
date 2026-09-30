package vsock

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no native AF_VSOCK: the virtio-win viosock driver provides it through a Winsock
// service provider (viosocklib.dll, registered by viosockwspsvc). The values below come from
// https://github.com/virtio-win/kvm-guest-drivers-windows at 04e70e76c966fd9247a523e41c1b6c624e9b46eb:
// viosock/inc/vio_sockets.h (VIOSOCK_NAME, IOCTL_GET_AF, struct sockaddr_vm, VMADDR_CID_ANY)
// and viosock/sys/public.h (AF_VSOCK 40, which IOCTL_GET_AF currently returns).
const (
	viosockDeviceName = `\??\Viosock`
	ioctlGetAF        = 0x0801300C

	vmaddrCIDAny = 0xFFFFFFFF

	// winsockVersion is Winsock 2.2, MAKEWORD(2, 2).
	winsockVersion = 0x0202
)

// sockaddrVM mirrors viosock's struct sockaddr_vm, a 12-byte structure with host byte order fields:
//
//	ADDRESS_FAMILY svm_family; USHORT svm_reserved1; UINT svm_port; UINT svm_cid;
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
}

var (
	winsockStartup = sync.OnceValue(func() error {
		var data windows.WSAData

		return windows.WSAStartup(winsockVersion, &data)
	})

	// x/sys/windows can't express custom socket address families, so call these directly.
	modws2_32       = windows.NewLazySystemDLL("ws2_32.dll")
	procBind        = modws2_32.NewProc("bind")
	procAccept      = modws2_32.NewProc("accept")
	procGetpeername = modws2_32.NewProc("getpeername")
)

// viosockAddressFamily asks the viosock driver for its address family,
// like ViosockGetAF() in vio_sockets.h.
func viosockAddressFamily() (uint16, error) {
	device, err := windows.CreateFile(windows.StringToUTF16Ptr(viosockDeviceName), windows.GENERIC_READ,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open the viosock device, is the virtio-win viosock driver "+
			"installed? %w", err)
	}
	defer windows.CloseHandle(device)

	var addressFamily, returned uint32
	if err := windows.DeviceIoControl(device, ioctlGetAF, nil, 0, (*byte)(unsafe.Pointer(&addressFamily)),
		uint32(unsafe.Sizeof(addressFamily)), &returned, nil); err != nil {
		return 0, fmt.Errorf("failed to query the viosock address family: %w", err)
	}
	if returned != uint32(unsafe.Sizeof(addressFamily)) || addressFamily == windows.AF_UNSPEC ||
		addressFamily > math.MaxUint16 {
		return 0, fmt.Errorf("viosock returned an invalid address family %d", addressFamily)
	}

	return uint16(addressFamily), nil
}

func bind(socket windows.Handle, sa *sockaddrVM) error {
	ret, _, err := procBind.Call(uintptr(socket), uintptr(unsafe.Pointer(sa)), unsafe.Sizeof(*sa))
	if int32(ret) != 0 {
		return fmt.Errorf("bind: %w", err)
	}

	return nil
}

func accept(socket windows.Handle) (windows.Handle, error) {
	// Accept into a buffer as large as a sockaddr_storage in case the
	// provider reports a larger address than it accepted in bind()
	var buf [128]byte
	length := int32(len(buf))

	ret, _, err := procAccept.Call(uintptr(socket), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&length)))
	if windows.Handle(ret) == windows.InvalidHandle {
		return windows.InvalidHandle, fmt.Errorf("accept: %w", err)
	}

	return windows.Handle(ret), nil
}

func getpeername(socket windows.Handle) (*sockaddrVM, error) {
	var buf [128]byte
	length := int32(len(buf))

	ret, _, err := procGetpeername.Call(uintptr(socket), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&length)))
	if int32(ret) != 0 {
		return nil, fmt.Errorf("getpeername: %w", err)
	}

	return parseSockaddrVM(buf[:length])
}

func parseSockaddrVM(buf []byte) (*sockaddrVM, error) {
	if len(buf) < int(unsafe.Sizeof(sockaddrVM{})) {
		return nil, fmt.Errorf("vsock address is %d bytes, expected at least %d", len(buf),
			unsafe.Sizeof(sockaddrVM{}))
	}

	return &sockaddrVM{
		Family:    binary.LittleEndian.Uint16(buf[0:2]),
		Reserved1: binary.LittleEndian.Uint16(buf[2:4]),
		Port:      binary.LittleEndian.Uint32(buf[4:8]),
		CID:       binary.LittleEndian.Uint32(buf[8:12]),
	}, nil
}
