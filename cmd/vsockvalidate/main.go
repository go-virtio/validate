// Headless go-virtio vsock validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-vsock device through the go-virtio/vsock driver, and
// attempts a packet round-trip with the host. The guest reports
// VSOCK-VALIDATE: PASS/FAIL on the serial console; run-vsock.sh parses it.
//
// IMPORTANT — HOST WALL: virtio-vsock's PCI front-end on QEMU is
// `vhost-vsock-pci`, which is backed by the HOST kernel's vhost_vsock module
// (AF_VSOCK / /dev/vhost-vsock). That kernel facility is Linux-only; macOS has
// no host vsock and the Homebrew macOS QEMU build does not even compile the
// device in. On this host run-vsock.sh therefore never reaches the guest — it
// maps the wall at the QEMU device-instantiation layer. This main() is the
// guest side that WOULD run on a Linux KVM host with vhost_vsock loaded; it is
// kept (and kept compiling) so the harness is complete and portable.
//
// What the guest would prove on a Linux host (any failure => FAIL):
//
//   - INIT: OpenVirtioVsock binds DID 0x1053, negotiates VERSION_1, reads the
//     assigned guest_cid from device config, and sets up rx/tx/event queues
//     with rx + event pre-posted.
//   - GUEST_CID: the assigned CID is a real, non-reserved value (== the
//     guest-cid passed to vhost-vsock-pci on the host).
//   - SEND: a TypeStream OpRequest (connection request) to (CIDHost, port)
//     marshals and transmits off the tx queue without ErrTransmitTimeout —
//     the device consumed the descriptor.
//   - RECEIVE: the host side (run-vsock.sh's AF_VSOCK listener) accepts and
//     replies; the guest receives a packet whose dst_cid == its own guest_cid
//     (the packet round-tripped back to us). This is the "packet actually
//     round-tripped" proof.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	"github.com/go-virtio/vsock"
)

const (
	// hostPort is the AF_VSOCK port the host-side listener binds (matches
	// run-vsock.sh).
	hostPort uint32 = 9999

	// guestPort is the ephemeral source port the guest advertises.
	guestPort uint32 = 1024

	// rxPoll is the busy-poll budget for ReceivePacket. vsock round-trips
	// are sub-millisecond on a real vhost backend; this is generous.
	rxPoll = 2000000
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernVsock)
	if dev == nil {
		// On a host without vhost_vsock (e.g. macOS) the device never
		// instantiates, so the guest would not even reach here — but if it
		// does, report the absence honestly rather than hanging silently.
		fmt.Printf("VSOCK-VALIDATE: FAIL no virtio-vsock-pci device found (host lacks vhost_vsock?)\n")
		halt()
	}

	t := transport.New(dev)

	v, err := vsock.OpenVirtioVsock(t)
	if err != nil {
		fmt.Printf("VSOCK-VALIDATE: FAIL OpenVirtioVsock: %v\n", err)
		halt()
	}

	fmt.Printf("VSOCK-VALIDATE: VSOCK=%#04x:%#04x guest_cid=%d features=%#x\n",
		dev.Vendor, dev.Device, v.GuestCID, v.NegotiatedFeatures)

	if v.GuestCID < 3 {
		// CIDs 0,1,2 are reserved (hypervisor/local/host). A real assigned
		// guest CID is >= 3.
		fmt.Printf("VSOCK-VALIDATE: FAIL guest_cid=%d is reserved (no real CID assigned)\n", v.GuestCID)
		halt()
	}

	// SEND: a stream connection request toward the host.
	req := vsock.Packet{
		SrcCID:  v.GuestCID,
		DstCID:  vsock.CIDHost,
		SrcPort: guestPort,
		DstPort: hostPort,
		Type:    vsock.TypeStream,
		Op:      vsock.OpRequest,
	}
	if err := v.SendPacket(req); err != nil {
		fmt.Printf("VSOCK-VALIDATE: FAIL SendPacket(OpRequest): %v\n", err)
		halt()
	}
	fmt.Printf("VSOCK-VALIDATE: sent OpRequest src=%d:%d dst=%d:%d (tx descriptor consumed)\n",
		req.SrcCID, req.SrcPort, req.DstCID, req.DstPort)

	// RECEIVE: the host replies; the packet must round-trip back to our CID.
	resp, err := v.ReceivePacket(rxPoll)
	if err != nil {
		fmt.Printf("VSOCK-VALIDATE: FAIL ReceivePacket: %v\n", err)
		halt()
	}
	fmt.Printf("VSOCK-VALIDATE: recv op=%d src_cid=%d dst_cid=%d src_port=%d dst_port=%d len=%d\n",
		resp.Op, resp.SrcCID, resp.DstCID, resp.SrcPort, resp.DstPort, len(resp.Data))

	if resp.DstCID != v.GuestCID {
		fmt.Printf("VSOCK-VALIDATE: FAIL reply dst_cid=%d is not our guest_cid=%d (did not round-trip to us)\n",
			resp.DstCID, v.GuestCID)
		halt()
	}

	fmt.Printf("VSOCK-VALIDATE: PASS packet round-tripped: sent OpRequest, received reply addressed to guest_cid=%d\n",
		v.GuestCID)
	halt()
}

func halt() {
	for {
	}
}
