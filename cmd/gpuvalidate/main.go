// Headless go-virtio validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-gpu-pci device through the go-virtio/gpu 2D
// framebuffer driver, renders a soft3d cube into the framebuffer, flushes
// it to the host scanout, and reports a pixel checksum over the serial
// console. The screendump-based pixel validation is performed by the host
// harness (run.sh) against the QEMU monitor.
//
// NOTE: an earlier version of this harness capped the framebuffer at 96x72
// because the guest appeared to "#DF after a fixed compute budget" under QEMU
// TCG. That was a misdiagnosis: it was not a CPU double fault but the legacy
// 8259 PIT (IRQ0) firing on IDT vector 8 (the #DF slot) because the board
// never masked the legacy PIC while routing through the I/O APIC. The board
// now masks the PIC (board.Init: outb(0x21, 0xff)), so sustained compute is
// safe and the framebuffer runs at full resolution.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	"github.com/go-virtio/gpu"
	"github.com/go-virtio/gpu/soft3d"
)

// fbWidth/fbHeight are the framebuffer dimensions. The former TCG compute
// budget (which capped this at 96x72) was the legacy PIT firing on IDT vector
// 8; the board now masks the 8259 PIC, so sustained compute is safe.
const (
	fbWidth  = 320
	fbHeight = 240
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernGPU)
	if dev == nil {
		fmt.Printf("VALIDATE: FAIL no virtio-gpu-pci device found\n")
		halt()
	}

	t := transport.New(dev)

	g, err := gpu.OpenVirtioGPU(t)
	if err != nil {
		fmt.Printf("VALIDATE: FAIL OpenVirtioGPU: %v\n", err)
		halt()
	}

	if _, err := g.DisplayInfo(); err != nil {
		fmt.Printf("VALIDATE: FAIL DisplayInfo: %v\n", err)
		halt()
	}

	fb, err := g.SetupFramebuffer(0, fbWidth, fbHeight)
	if err != nil {
		fmt.Printf("VALIDATE: FAIL SetupFramebuffer: %v\n", err)
		halt()
	}

	// Render the soft3d cube into the BGRA framebuffer, then flush. This is
	// the first heavy work; it must complete before the TCG compute budget
	// is exhausted.
	soft3d.RenderCube(fb.Pix, int(fb.Width), int(fb.Height), 0.6)

	if err := fb.Flush(); err != nil {
		fmt.Printf("VALIDATE: FAIL Flush: %v\n", err)
		halt()
	}

	// Report after the work, so the host harness can read the checksum and
	// then screendump.
	sum, nonZero, distinct := pixStats(fb.Pix)
	fmt.Printf("VALIDATE: GPU=%#04x:%#04x scanouts=%d features=%#x\n",
		dev.Vendor, dev.Device, g.NumScanouts, g.NegotiatedFeatures)
	fmt.Printf("VALIDATE: fb %dx%d resource=%d pixbytes=%d\n",
		fb.Width, fb.Height, fb.ResourceID, len(fb.Pix))
	fmt.Printf("VALIDATE: checksum=%#08x nonzero_pixels=%d distinct_colors=%d\n",
		sum, nonZero, distinct)
	fmt.Printf("VALIDATE: DONE render+flush complete; host may now screendump\n")

	halt()
}

// pixStats returns a rolling checksum over the BGRA bytes, the count of
// non-background pixels (background is the soft3d clear color {10,10,20}),
// and the number of distinct 32-bit pixel values (capped) — enough to
// assert the frame is not uniform.
func pixStats(pix []byte) (sum uint32, nonBg int, distinct int) {
	seen := make(map[uint32]struct{}, 256)
	for i := 0; i+3 < len(pix); i += 4 {
		b, gr, r := pix[i], pix[i+1], pix[i+2]
		p := uint32(b) | uint32(gr)<<8 | uint32(r)<<16 | uint32(pix[i+3])<<24
		sum = sum*31 + p
		if !(b == 20 && gr == 10 && r == 10) {
			nonBg++
		}
		if len(seen) < 4096 {
			seen[p] = struct{}{}
		}
	}
	return sum, nonBg, len(seen)
}

func halt() {
	for {
	}
}
