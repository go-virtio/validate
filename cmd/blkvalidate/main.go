// Headless go-virtio blk validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-blk-pci device through the go-virtio/blk driver, and
// performs an entirely in-guest write/read-back equality proof over a scratch
// raw disk. No host-side observation is needed — the guest writes a
// deterministic, LBA-stamped pattern to several sectors (including a non-zero
// LBA and a multi-block run), Flushes, reads them back, asserts byte-equality
// itself, and reports BLK-VALIDATE: ... PASS/FAIL plus a checksum over the
// serial console. run-blk.sh parses the verdict and exits non-zero on FAIL.
//
// This mirrors the gpu harness: a *real* virtio-blk-pci device (not the fake
// transport the unit tests use) is what exercises the descriptor-chain build,
// the device-written status byte, capacity/devcfg reads, and DMA alignment on
// actual QEMU emulation.
//
// The 8 MiB scratch image (run-blk.sh creates it) is 8*1024*1024/512 = 16384
// sectors; the harness sanity-checks the reported capacity against that.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/blk"
	"github.com/go-virtio/common"
)

// expectedSectors is 8 MiB / 512 — the scratch image run-blk.sh creates.
const expectedSectors uint64 = 8 * 1024 * 1024 / blk.BlockSize

// Test LBAs. We deliberately include LBA 0 (start), a non-zero single-block
// LBA, and a multi-block run at a higher non-zero LBA so a single round-trip
// can catch off-by-one sector addressing and multi-descriptor data handling.
const (
	lbaZero  uint64 = 0
	lbaMid   uint64 = 1234 // arbitrary non-zero single-block sector
	lbaRun   uint64 = 8000 // start of a multi-block run
	runCount        = 5    // sectors in the multi-block run (5*512 = 2560 B)
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernBlock)
	if dev == nil {
		fmt.Printf("BLK-VALIDATE: FAIL no virtio-blk-pci device found\n")
		halt()
	}

	t := transport.New(dev)

	b, err := blk.OpenVirtioBlk(t)
	if err != nil {
		fmt.Printf("BLK-VALIDATE: FAIL OpenVirtioBlk: %v\n", err)
		halt()
	}

	fmt.Printf("BLK-VALIDATE: BLK=%#04x:%#04x capacity_sectors=%d ro=%v features=%#x\n",
		dev.Vendor, dev.Device, b.Capacity, b.ReadOnly, b.NegotiatedFeatures)

	// Capacity sanity-check against the known 8 MiB scratch image.
	if b.Capacity != expectedSectors {
		fmt.Printf("BLK-VALIDATE: FAIL capacity mismatch: got %d want %d sectors\n",
			b.Capacity, expectedSectors)
		halt()
	}
	if b.ReadOnly {
		fmt.Printf("BLK-VALIDATE: FAIL device is read-only; cannot run write/read-back proof\n")
		halt()
	}

	var sum uint64

	// Case 1: single block at LBA 0.
	if ok := writeReadBack(b, lbaZero, 1, &sum); !ok {
		halt()
	}
	// Case 2: single block at a non-zero LBA.
	if ok := writeReadBack(b, lbaMid, 1, &sum); !ok {
		halt()
	}
	// Case 3: a multi-block run at a higher non-zero LBA (exercises a data
	// descriptor spanning multiple sectors).
	if ok := writeReadBack(b, lbaRun, runCount, &sum); !ok {
		halt()
	}

	// Independent isolation check: re-read LBA 0 after the later writes and
	// confirm the multi-block run at lbaRun did NOT clobber it (catches a
	// driver that ignores the sector field on writes).
	got, err := b.ReadBlocks(lbaZero, 1)
	if err != nil {
		fmt.Printf("BLK-VALIDATE: FAIL re-read LBA %d: %v\n", lbaZero, err)
		halt()
	}
	want := pattern(lbaZero, 1)
	if !equal(got, want) {
		fmt.Printf("BLK-VALIDATE: FAIL LBA %d was clobbered by a later write (sector field ignored?)\n", lbaZero)
		halt()
	}

	fmt.Printf("BLK-VALIDATE: checksum=%#016x\n", sum)
	fmt.Printf("BLK-VALIDATE: PASS write/read-back byte-equal at LBAs %d, %d, %d..%d (Flush OK)\n",
		lbaZero, lbaMid, lbaRun, lbaRun+runCount-1)
	halt()
}

// writeReadBack writes the LBA-stamped pattern for [sector, sector+count),
// Flushes, reads it back, and asserts byte-equality. It folds the verified
// bytes into *sum. Returns false (after printing a FAIL line) on any mismatch
// or driver error.
func writeReadBack(b *blk.VirtioBlk, sector uint64, count int, sum *uint64) bool {
	want := pattern(sector, count)

	if err := b.WriteBlocks(sector, want); err != nil {
		fmt.Printf("BLK-VALIDATE: FAIL WriteBlocks(LBA %d, %d blk): %v\n", sector, count, err)
		return false
	}
	if err := b.Flush(); err != nil {
		fmt.Printf("BLK-VALIDATE: FAIL Flush after LBA %d: %v\n", sector, err)
		return false
	}
	got, err := b.ReadBlocks(sector, count)
	if err != nil {
		fmt.Printf("BLK-VALIDATE: FAIL ReadBlocks(LBA %d, %d blk): %v\n", sector, count, err)
		return false
	}
	if len(got) != len(want) {
		fmt.Printf("BLK-VALIDATE: FAIL LBA %d length: got %d want %d\n", sector, len(got), len(want))
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			fmt.Printf("BLK-VALIDATE: FAIL LBA %d byte %d: got %#02x want %#02x\n",
				sector, i, got[i], want[i])
			return false
		}
	}
	for _, c := range got {
		*sum = *sum*1099511628211 + uint64(c) // 64-bit FNV-ish fold
	}
	fmt.Printf("BLK-VALIDATE: ok LBA %d count=%d bytes=%d round-trip byte-equal\n",
		sector, count, len(got))
	return true
}

// pattern builds a deterministic, LBA-stamped buffer of count 512-byte
// sectors starting at `sector`. Each byte is derived from the absolute sector
// index and the in-sector offset, so reading the wrong sector — or a sector
// shifted by one — yields different bytes and is caught.
func pattern(sector uint64, count int) []byte {
	buf := make([]byte, count*blk.BlockSize)
	for s := 0; s < count; s++ {
		lba := sector + uint64(s)
		base := s * blk.BlockSize
		for off := 0; off < blk.BlockSize; off++ {
			// Mix the LBA (so the sector identity is encoded) with the
			// in-sector offset (so the byte position matters).
			buf[base+off] = byte(lba*31 + uint64(off)*7 + 0xA5)
		}
	}
	return buf
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func halt() {
	for {
	}
}
