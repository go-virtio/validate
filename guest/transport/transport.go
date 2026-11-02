// Package transport bridges go-virtio/common.Transport to tamago's amd64
// PCI + DMA facilities so the transport-agnostic virtio drivers can drive a
// real virtio-*-pci device under QEMU. It is shared by every harness command
// (cmd/gpuvalidate, cmd/blkvalidate, …).
//
// Three sub-interfaces (see go-virtio/common/transport.go):
//
//   - PCIConfigReader: routed to tamago's soc/intel/pci.Device, which
//     performs port-mapped (0xcf8/0xcfc) PCI configuration-space reads.
//
//   - BARMemoryAccessor: routed to direct MMIO. tamago's amd64 init.s
//     identity-maps the full 32-bit space plus a 2 GiB 64-bit window
//     (0x100000000..0x17fffffff), with the PCI MMIO hole
//     (0xc0000000..0xffffffff) and the 64-bit window mapped uncacheable.
//     A virtio BAR therefore resolves to a plain physical address we read
//     and write through an unsafe.Pointer.
//
//   - PageAllocator: routed to tamago's dma.Reserve, which carves
//     page-aligned, physically-contiguous, identity-mapped buffers out of
//     the board's global DMA region.
package transport

import (
	"unsafe"

	"github.com/usbarmory/tamago/dma"
	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
)

// pageSize mirrors common.PageSize (4 KiB) for the DMA allocator.
const pageSize = 4096

// Transport implements common.Transport for one PCI virtio device.
type Transport struct {
	dev *pci.Device

	// barBase[i] is the physical base address of BAR i, decoded once at
	// construction from the device's BAR registers. Zero means the BAR
	// is unused / unmapped.
	barBase [6]uint64
}

// New builds a transport for dev. It enables PCI Memory Space (so BAR MMIO
// decodes) and Bus Master (so the device can DMA into our virtqueue /
// backing buffers) then snapshots every BAR base.
func New(dev *pci.Device) *Transport {
	// PCI Command register: bit1 = Memory Space Enable, bit2 = Bus Master
	// Enable. Both are required for a DMA-capable MMIO virtio device.
	cmd := dev.Read(0, pci.Command)
	dev.Write(0, pci.Command, cmd|(1<<1)|(1<<2))

	t := &Transport{dev: dev}
	for i := 0; i < 6; i++ {
		t.barBase[i] = uint64(dev.BaseAddress(i))
	}
	return t
}

// --- PCIConfigReader ---------------------------------------------------
//
// tamago's pci.Device.Read(fn, off) selects the dword at (off & 0xfc) but
// only post-shifts the result by (off&2)*8 — it resolves 16-bit but NOT
// 8-bit (odd-byte) granularity, and its own address masks off the low two
// bits. The virtio cap walker reads single bytes at odd offsets (Next at
// +1, cfg_type at +3, id at +5), so we read the aligned dword ourselves
// and extract the requested byte / word from the full low two bits.

// dword returns the 32-bit config-space dword containing offset, aligned.
func (t *Transport) dword(offset uint8) uint32 {
	// Pass an already-aligned offset so tamago's internal (off&2)*8 shift
	// is zero and we get the raw little-endian dword back.
	return t.dev.Read(0, uint32(offset)&0xfc)
}

func (t *Transport) ReadConfig8(offset uint8) (uint8, error) {
	v := t.dword(offset)
	shift := (uint32(offset) & 3) * 8
	return uint8((v >> shift) & 0xff), nil
}

func (t *Transport) ReadConfig16(offset uint8) (uint16, error) {
	v := t.dword(offset)
	shift := (uint32(offset) & 2) * 8
	return uint16((v >> shift) & 0xffff), nil
}

func (t *Transport) ReadConfig32(offset uint8) (uint32, error) {
	return t.dword(offset), nil
}

// --- BARMemoryAccessor -------------------------------------------------

// addr resolves (bar, offset) to a physical MMIO address.
func (t *Transport) addr(bar uint8, offset uint64) (uintptr, error) {
	if bar > 5 {
		return 0, errBadBAR
	}
	base := t.barBase[bar]
	if base == 0 {
		return 0, errBadBAR
	}
	return uintptr(base + offset), nil
}

func (t *Transport) Read8(bar uint8, offset uint64) (uint8, error) {
	a, err := t.addr(bar, offset)
	if err != nil {
		return 0, err
	}
	return *(*uint8)(unsafe.Pointer(a)), nil
}

func (t *Transport) Read16(bar uint8, offset uint64) (uint16, error) {
	a, err := t.addr(bar, offset)
	if err != nil {
		return 0, err
	}
	return *(*uint16)(unsafe.Pointer(a)), nil
}

func (t *Transport) Read32(bar uint8, offset uint64) (uint32, error) {
	a, err := t.addr(bar, offset)
	if err != nil {
		return 0, err
	}
	return *(*uint32)(unsafe.Pointer(a)), nil
}

func (t *Transport) Read64(bar uint8, offset uint64) (uint64, error) {
	// Decompose into two 32-bit reads (low then high) — robust against
	// MMIO windows that dislike 64-bit accesses.
	lo, err := t.Read32(bar, offset)
	if err != nil {
		return 0, err
	}
	hi, err := t.Read32(bar, offset+4)
	if err != nil {
		return 0, err
	}
	return uint64(hi)<<32 | uint64(lo), nil
}

func (t *Transport) Write8(bar uint8, offset uint64, val uint8) error {
	a, err := t.addr(bar, offset)
	if err != nil {
		return err
	}
	*(*uint8)(unsafe.Pointer(a)) = val
	return nil
}

func (t *Transport) Write16(bar uint8, offset uint64, val uint16) error {
	a, err := t.addr(bar, offset)
	if err != nil {
		return err
	}
	*(*uint16)(unsafe.Pointer(a)) = val
	return nil
}

func (t *Transport) Write32(bar uint8, offset uint64, val uint32) error {
	a, err := t.addr(bar, offset)
	if err != nil {
		return err
	}
	*(*uint32)(unsafe.Pointer(a)) = val
	return nil
}

func (t *Transport) Write64(bar uint8, offset uint64, val uint64) error {
	if err := t.Write32(bar, offset, uint32(val)); err != nil {
		return err
	}
	return t.Write32(bar, offset+4, uint32(val>>32))
}

// --- PageAllocator -----------------------------------------------------

// AllocatePages reserves count page-aligned, physically-contiguous,
// identity-mapped pages from tamago's global DMA region and zeroes them
// (the drivers rely on a zeroed used-ring idx).
func (t *Transport) AllocatePages(count int) (uint64, []byte, error) {
	if count <= 0 {
		return 0, nil, errBadAlloc
	}
	addr, buf := dma.Reserve(count*pageSize, pageSize)
	if addr == 0 || buf == nil {
		return 0, nil, errBadAlloc
	}
	for i := range buf {
		buf[i] = 0
	}
	return uint64(addr), buf, nil
}

// compile-time assertion that *Transport satisfies the interface.
var _ common.Transport = (*Transport)(nil)

type transportError string

func (e transportError) Error() string { return string(e) }

const (
	errBadBAR   = transportError("validate: BAR index out of range or unmapped")
	errBadAlloc = transportError("validate: DMA page allocation failed")
)
