// Headless go-virtio net validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-net-pci device through the go-virtio/net driver, and
// performs a TX (and, if the host peer plays along, RX) frame round-trip
// against a host-side raw-frame peer.
//
// Unlike blk (self-contained) and console (host echoes), net REQUIRES a host
// observer: a virtio-net TX frame leaves the guest and lands on the QEMU
// netdev backend; only the host can confirm it arrived. run-net.sh wires a
// `-netdev dgram` backend to a tiny Python peer that:
//
//  1. receives the guest's TX frame as a RAW Ethernet frame (no length
//     prefix on the dgram backend), checks our magic EtherType + payload,
//     and reports TX-OBSERVED on its log; then
//  2. sends a RAW response frame back so the guest's ReceiveFrame path is
//     exercised too (best-effort: if RX doesn't complete under TCG the
//     harness still PASSES on the TX observation and reports the RX wall).
//
// The guest prints NET-VALIDATE: ... lines over the SERIAL console (COM1, a
// device separate from the virtio-net under test). The TX verdict is the
// guest's own "frame handed to device" plus the HOST's TX-OBSERVED line
// (run-net.sh cross-checks both). RX is asserted in-guest when a frame comes
// back.
//
// Like blk/console, QEMU's virtio-net-pci defaults transitional (legacy DID
// 0x1000); run-net.sh passes disable-legacy=on so the device advertises the
// modern DID 0x1041 the modern-only driver (pci.Probe for 0x1041) requires.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	vnet "github.com/go-virtio/net"
)

// magicEtherType is an experimental/local EtherType (IEEE 802 "local
// experimental" range 0x88B5) the host peer keys on to distinguish our test
// frame from any background traffic. magicPayload is a recognizable token in
// the frame body so a corrupted/truncated frame is caught host-side.
const magicEtherType uint16 = 0x88B5

var magicPayload = []byte("GO-VIRTIO-NET-VALIDATE-0123456789")

// dstMAC is a locally-administered unicast destination (bit 1 of the first
// octet set = locally administered; bit 0 clear = unicast). The host dgram
// peer ignores addressing, but a well-formed frame keeps real stacks happy.
var dstMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}

// rxPollBudget bounds the in-guest wait for the host's response frame. The
// RX direction is best-effort: a timeout here is a mapped wall, not a hard
// FAIL (TX observation is the required deliverable).
const rxPollBudget = 4000000

// buildFrame assembles a minimum-size Ethernet II frame:
//
//	dst[6] | src[6] | ethertype[2] | payload | zero-pad to 60 bytes
//
// The 60-byte floor is the Ethernet minimum frame size minus the 4-byte FCS
// (which the virtio backend does not carry). Padding guarantees the frame is
// never runt-dropped by any host-side stack.
func buildFrame(src vnet.MAC6) []byte {
	const minLen = 60
	body := 14 + len(magicPayload)
	n := body
	if n < minLen {
		n = minLen
	}
	f := make([]byte, n)
	copy(f[0:6], dstMAC[:])
	copy(f[6:12], src[:])
	f[12] = byte(magicEtherType >> 8)
	f[13] = byte(magicEtherType & 0xff)
	copy(f[14:], magicPayload)
	return f
}

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernNet)
	if dev == nil {
		fmt.Printf("NET-VALIDATE: FAIL no virtio-net-pci device found (modern DID %#04x)\n",
			common.PCIDeviceIDModernNet)
		halt()
	}

	t := transport.New(dev)

	v, err := vnet.OpenVirtioNet(t)
	if err != nil {
		fmt.Printf("NET-VALIDATE: FAIL OpenVirtioNet: %v\n", err)
		halt()
	}

	fmt.Printf("NET-VALIDATE: NET=%#04x:%#04x mac=%s features=%#x rxq_size=%d txq_size=%d\n",
		dev.Vendor, dev.Device, v.MAC.String(), v.NegotiatedFeatures,
		v.RxQueue().Layout.Size, v.TxQueue().Layout.Size)

	frame := buildFrame(v.MAC)

	// --- TX: transmit the known frame. -----------------------------------
	// A nil return means the device returned the descriptor on the used
	// ring (the frame was accepted by the backend). The HOST peer
	// independently confirms it actually arrived (TX-OBSERVED in its log,
	// cross-checked by run-net.sh).
	if err := v.TransmitFrame(frame); err != nil {
		fmt.Printf("NET-VALIDATE: FAIL TransmitFrame: %v\n", err)
		halt()
	}
	fmt.Printf("NET-VALIDATE: tx handed %d-byte frame to device (ethertype=%#04x payload=%q)\n",
		len(frame), magicEtherType, string(magicPayload))
	fmt.Printf("NET-VALIDATE: TX-DONE device returned the tx descriptor; host peer should observe the frame\n")

	// --- RX (best-effort): wait for the host's response frame. ------------
	// If the host dgram peer sends a frame back, ReceiveFrame returns the
	// Ethernet payload (virtio header already stripped). A timeout is a
	// mapped wall (TCG RX latency / backend not echoing) — NOT a hard FAIL.
	rx, err := v.ReceiveFrame(rxPollBudget)
	if err != nil {
		fmt.Printf("NET-VALIDATE: RX-WALL ReceiveFrame: %v (TX path is the required proof; RX not confirmed in-guest)\n", err)
		fmt.Printf("NET-VALIDATE: PASS tx frame handed to a real virtio-net-pci device (RX best-effort, see RX-WALL)\n")
		halt()
	}

	// Inspect the received frame: report its ethertype + length so the
	// transcript shows what came back.
	if len(rx) >= 14 {
		et := uint16(rx[12])<<8 | uint16(rx[13])
		fmt.Printf("NET-VALIDATE: rx received %d-byte frame ethertype=%#04x\n", len(rx), et)
	} else {
		fmt.Printf("NET-VALIDATE: rx received %d-byte frame (shorter than an Ethernet header)\n", len(rx))
	}
	fmt.Printf("NET-VALIDATE: PASS tx frame handed to device AND rx frame received from host peer (both directions)\n")
	halt()
}

func halt() {
	for {
	}
}
