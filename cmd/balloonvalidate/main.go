// Headless go-virtio balloon validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-balloon-pci device through the go-virtio/balloon driver,
// and proves the device actually consumes the inflate/deflate PFN buffers off
// the used ring. The guest reports BALLOON-VALIDATE: PASS/FAIL on the serial
// console; run-balloon.sh parses the verdict and also queries the QEMU monitor
// `info balloon` before/after as a host-side cross-check.
//
// This mirrors the blk harness: a *real* virtio-balloon-pci device (not the
// fake transport the unit tests use) exercises the inflateq/deflateq bring-up,
// the PFN-array marshalling, the doorbell, and the used-ring consumption on
// actual QEMU emulation.
//
// What the guest proves (any failure => FAIL):
//
//   - INIT: OpenVirtioBalloon binds DID 0x1045, negotiates VERSION_1, reads
//     num_pages from device config, and sets up exactly two queues.
//   - INFLATE: posting nPages PFNs to inflateq returns without
//     ErrRequestTimeout — i.e. the device CONSUMED the buffer off the used
//     ring (a dead device would time out). Actual advances by nPages.
//   - DEFLATE: posting the same PFNs to deflateq likewise returns (device
//     consumed it). Actual returns to zero.
//   - ROUND 2: a second inflate of a different size also consumes, proving
//     the queues keep working across requests.
//
// HOST-SIDE NOTE: this driver deliberately does NOT write the balloon `actual`
// field back to device config (common.ModernConfig exposes no device-config
// write path — a documented limitation), so the host's `info balloon` "actual"
// reflects the host-set TARGET, not the guest's guest-initiated inflate. The
// authoritative guest-side proof of the balloon mechanism is therefore the
// used-ring consumption asserted here; run-balloon.sh prints `info balloon` for
// transparency but does not gate on it.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/balloon"
	"github.com/go-virtio/common"
)

const (
	// inflatePages is the first inflate size. 64 pages = 256 KiB, posted as
	// a single PFN buffer (well under MaxPFNsPerBuffer = 256).
	inflatePages = 64

	// inflatePages2 is the second-round inflate size, deliberately larger
	// than MaxPFNsPerBuffer (256) so the driver's multi-buffer split path is
	// exercised against the real device. 300 PFNs => two posted buffers.
	inflatePages2 = 300
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernBalloon)
	if dev == nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL no virtio-balloon-pci device found\n")
		halt()
	}

	t := transport.New(dev)

	b, err := balloon.OpenVirtioBalloon(t)
	if err != nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL OpenVirtioBalloon: %v\n", err)
		halt()
	}

	fmt.Printf("BALLOON-VALIDATE: BALLOON=%#04x:%#04x num_pages=%d features=%#x actual=%d\n",
		dev.Vendor, dev.Device, b.NumPages, b.NegotiatedFeatures, b.Actual)

	if b.InflateQueue() == nil || b.DeflateQueue() == nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL inflate/deflate queue not set up\n")
		halt()
	}

	// INFLATE round 1: hand inflatePages guest pages to the host. A return
	// without error proves the device consumed the PFN buffer off the used
	// ring (postChunk busy-polls and returns ErrRequestTimeout on a dead
	// device).
	if err := b.Inflate(inflatePages); err != nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL Inflate(%d): %v\n", inflatePages, err)
		halt()
	}
	if b.Actual != inflatePages {
		fmt.Printf("BALLOON-VALIDATE: FAIL after inflate Actual=%d want %d\n", b.Actual, inflatePages)
		halt()
	}
	fmt.Printf("BALLOON-VALIDATE: inflate1 ok pages=%d actual=%d (device consumed inflateq buffer)\n",
		inflatePages, b.Actual)

	// DEFLATE: reclaim the same pages. Again, a clean return proves the
	// device consumed the deflateq buffer.
	if err := b.Deflate(inflatePages); err != nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL Deflate(%d): %v\n", inflatePages, err)
		halt()
	}
	if b.Actual != 0 {
		fmt.Printf("BALLOON-VALIDATE: FAIL after deflate Actual=%d want 0\n", b.Actual)
		halt()
	}
	fmt.Printf("BALLOON-VALIDATE: deflate ok pages=%d actual=%d (device consumed deflateq buffer)\n",
		inflatePages, b.Actual)

	// INFLATE round 2: a larger inflate that spans more than one PFN buffer,
	// exercising the multi-buffer split path against the real device.
	if err := b.Inflate(inflatePages2); err != nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL Inflate(%d, multi-buffer): %v\n", inflatePages2, err)
		halt()
	}
	if b.Actual != inflatePages2 {
		fmt.Printf("BALLOON-VALIDATE: FAIL after inflate2 Actual=%d want %d\n", b.Actual, inflatePages2)
		halt()
	}
	fmt.Printf("BALLOON-VALIDATE: inflate2 ok pages=%d actual=%d (multi-buffer split consumed)\n",
		inflatePages2, b.Actual)

	// Clean up round 2 so the balloon ends deflated.
	if err := b.Deflate(inflatePages2); err != nil {
		fmt.Printf("BALLOON-VALIDATE: FAIL Deflate(%d) round 2: %v\n", inflatePages2, err)
		halt()
	}
	if b.Actual != 0 {
		fmt.Printf("BALLOON-VALIDATE: FAIL after final deflate Actual=%d want 0\n", b.Actual)
		halt()
	}

	fmt.Printf("BALLOON-VALIDATE: PASS inflate/deflate consumed off the used ring (pages %d then %d, multi-buffer; back to actual=0)\n",
		inflatePages, inflatePages2)
	halt()
}

func halt() {
	for {
	}
}
