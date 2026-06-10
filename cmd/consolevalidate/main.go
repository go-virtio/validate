// Headless go-virtio console validate harness: boots tamago/amd64 under
// QEMU, drives a real virtio-console (virtio-serial-pci + virtconsole)
// device through the go-virtio/console driver, and performs a full TX→RX
// round-trip proof against a host-side echo peer.
//
// Topology (run-console.sh wires it):
//
//	guest virtio-console  <--virtqueue-->  QEMU virtconsole
//	                                       chardev=socket (unix)
//	                                            |
//	                                       host echo server (Python)
//
// The guest WRITES a known multi-byte token to the console transmitq; QEMU
// hands it to the host chardev socket; the host echo server reads it and
// writes the exact same bytes back; QEMU lands them on the guest receiveq;
// the guest READS them and asserts byte-equality itself. It then prints
// CONSOLE-VALIDATE: PASS / FAIL over the SERIAL console (COM1) — which is a
// SEPARATE device from the virtio-console under test, so the verdict can't
// be confused with the token bytes. run-console.sh parses the verdict.
//
// This mirrors the blk/gpu harnesses: a *real* virtio-console-pci device
// (not the fake transport the unit tests use) is what exercises the
// two-virtqueue setup (rx=queue0, tx=queue1), the raw header-less byte
// stream, descriptor publication, used-ring polling, and DMA alignment on
// actual QEMU emulation.
//
// Like blk, QEMU's virtio-serial-pci defaults transitional (legacy DID
// 0x1003); run-console.sh passes disable-legacy=on so the device advertises
// the modern DID 0x1043 that the modern-only driver (pci.Probe for 0x1043)
// requires.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	"github.com/go-virtio/console"
)

// token is the known multi-byte payload the guest writes to the console and
// expects to read back, byte-for-byte, from the host echo peer. It is
// deliberately multi-byte and non-trivial (mixed letters/digits/punct) so a
// truncation, an off-by-one, or a byte-swap in the data path is caught.
var token = []byte("GO-VIRTIO-CONSOLE-RT-0123456789-abcdef\n")

// rxPollBudget is how many busy-poll iterations Read spends waiting for the
// echoed bytes to come back through the host peer + QEMU. The host echo is
// near-instant but the guest may need to spin a while under TCG; this is a
// generous bound. We loop the whole Read several times below because the
// device may split the echoed token across multiple receiveq buffers.
const rxPollBudget = 4000000

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernConsole)
	if dev == nil {
		fmt.Printf("CONSOLE-VALIDATE: FAIL no virtio-console-pci device found (modern DID %#04x)\n",
			common.PCIDeviceIDModernConsole)
		halt()
	}

	t := transport.New(dev)

	c, err := console.OpenVirtioConsole(t)
	if err != nil {
		fmt.Printf("CONSOLE-VALIDATE: FAIL OpenVirtioConsole: %v\n", err)
		halt()
	}

	fmt.Printf("CONSOLE-VALIDATE: CONSOLE=%#04x:%#04x features=%#x rxq_size=%d txq_size=%d\n",
		dev.Vendor, dev.Device, c.NegotiatedFeatures,
		c.ReceiveQueue().Layout.Size, c.TransmitQueue().Layout.Size)

	// --- TX: write the known token to the console output. -----------------
	n, err := c.Write(token)
	if err != nil {
		fmt.Printf("CONSOLE-VALIDATE: FAIL Write(token): %v (wrote %d/%d)\n", err, n, len(token))
		halt()
	}
	if n != len(token) {
		fmt.Printf("CONSOLE-VALIDATE: FAIL short write: wrote %d want %d\n", n, len(token))
		halt()
	}
	fmt.Printf("CONSOLE-VALIDATE: tx wrote %d bytes, awaiting host echo on rx\n", n)

	// --- RX: read the echoed bytes back. ----------------------------------
	// The host echo peer returns the exact same bytes; QEMU may deliver them
	// across more than one receiveq buffer, so accumulate until we have at
	// least len(token) bytes (or a Read times out, which is a FAIL).
	got := make([]byte, 0, len(token)*2)
	for len(got) < len(token) {
		chunk, err := c.Read(rxPollBudget)
		if err != nil {
			fmt.Printf("CONSOLE-VALIDATE: FAIL Read after %d/%d echoed bytes: %v\n",
				len(got), len(token), err)
			halt()
		}
		got = append(got, chunk...)
	}

	// --- Assert byte-equality of the first len(token) echoed bytes. -------
	if len(got) < len(token) {
		fmt.Printf("CONSOLE-VALIDATE: FAIL echoed length %d < token %d\n", len(got), len(token))
		halt()
	}
	for i := range token {
		if got[i] != token[i] {
			fmt.Printf("CONSOLE-VALIDATE: FAIL echoed byte %d: got %#02x want %#02x\n",
				i, got[i], token[i])
			halt()
		}
	}

	fmt.Printf("CONSOLE-VALIDATE: rx read %d bytes, first %d byte-equal to tx token\n",
		len(got), len(token))
	fmt.Printf("CONSOLE-VALIDATE: PASS console TX->host-echo->RX round-trip byte-equal (%d bytes)\n",
		len(token))
	halt()
}

func halt() {
	for {
	}
}
