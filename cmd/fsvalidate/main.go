// Headless go-virtio fs validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-fs (vhost-user-fs-pci) device through the
// go-virtio/fs driver against an out-of-process virtiofsd daemon, and
// performs a real FUSE round-trip:
//
//   - OpenVirtioFS -> Init (FUSE_INIT handshake with virtiofsd)
//   - Lookup(FUSE_ROOT, "hello.txt") -> Open -> Read, asserting the bytes
//     read back equal a known string the host placed in the shared dir.
//   - Create(FUSE_ROOT, "guest-wrote.txt") + Write of a known token, so the
//     host can confirm the guest's write reached the real shared directory.
//
// It prints FS-VALIDATE: ... PASS/FAIL plus the read-back bytes and the
// write token over the serial console. The host (run-fs.sh / the validate
// driver) parses the verdict and then independently checks that the
// guest-written file appeared in the shared dir with the expected token.
//
// This mirrors cmd/blkvalidate: a *real* virtio device (not the fake
// transport the unit tests use) exercises the descriptor-chain build, the
// device-written reply, virtio_fs_config tag read, and DMA alignment on
// actual QEMU emulation — here additionally crossing the vhost-user +
// shared-memory boundary into virtiofsd, which only Linux hosts can run.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	"github.com/go-virtio/fs"
)

// fuseRoot is the FUSE root node id (Linux fuse.h FUSE_ROOT_ID): the inode
// every path lookup starts from. The host's --shared-dir maps to it.
const fuseRoot uint64 = 1

// readName is the file the host (run-fs.sh) creates in the shared dir;
// readWant is its exact, byte-for-byte contents.
const (
	readName = "hello.txt"
	readWant = "GO-VIRTIO-FS-VALIDATE-OK"
)

// writeName / writeToken: the guest creates writeName and writes
// writeToken; the host confirms the file appeared with that token. The
// token is echoed over serial so the host check and the guest claim are
// cross-verifiable.
const (
	writeName  = "guest-wrote.txt"
	writeToken = "GUEST-WROTE-THIS-1A2B3C4D"
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernFS)
	if dev == nil {
		fmt.Printf("FS-VALIDATE: FAIL no virtio-fs-pci device found\n")
		halt()
	}

	t := transport.New(dev)

	f, err := fs.OpenVirtioFS(t)
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL OpenVirtioFS: %v\n", err)
		halt()
	}

	fmt.Printf("FS-VALIDATE: FS=%#04x:%#04x tag=%q num_request_queues=%d features=%#x\n",
		dev.Vendor, dev.Device, f.Tag, f.NumRequestQueues, f.NegotiatedFeatures)

	// FUSE handshake with virtiofsd.
	if err := f.Init(); err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Init (FUSE_INIT): %v\n", err)
		halt()
	}
	fmt.Printf("FS-VALIDATE: FUSE negotiated major=%d minor=%d flags=%#x\n",
		f.FuseMajor, f.FuseMinor, f.FuseFlags)

	// --- READ proof: read a known host-created file byte-for-byte. ---
	ent, err := f.Lookup(fuseRoot, readName)
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Lookup(%q): %v\n", readName, err)
		halt()
	}
	fmt.Printf("FS-VALIDATE: lookup %q nodeid=%d size=%d mode=%#o\n",
		readName, ent.NodeID, ent.Attr.Size, ent.Attr.Mode)

	if ent.NodeID == 0 {
		fmt.Printf("FS-VALIDATE: FAIL Lookup(%q) negative (file absent in shared dir)\n", readName)
		halt()
	}
	if ent.Attr.Size != uint64(len(readWant)) {
		fmt.Printf("FS-VALIDATE: FAIL size mismatch: got %d want %d\n",
			ent.Attr.Size, len(readWant))
		halt()
	}

	fh, err := f.Open(ent.NodeID)
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Open(%q): %v\n", readName, err)
		halt()
	}

	got, err := f.Read(ent.NodeID, fh, 0, uint32(len(readWant)))
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Read(%q): %v\n", readName, err)
		halt()
	}
	_ = f.Release(ent.NodeID, fh)

	fmt.Printf("FS-VALIDATE: read %q bytes=%d data=%q\n", readName, len(got), string(got))

	if string(got) != readWant {
		fmt.Printf("FS-VALIDATE: FAIL read-back mismatch: got %q want %q\n",
			string(got), readWant)
		halt()
	}

	// --- WRITE proof: create a file and write a token the host can check. ---
	wEnt, wfh, err := f.Create(fuseRoot, writeName, 0o644, fs.OpenReadWrite)
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Create(%q): %v\n", writeName, err)
		halt()
	}

	n, err := f.Write(wEnt.NodeID, wfh, 0, []byte(writeToken))
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Write(%q): %v\n", writeName, err)
		halt()
	}
	if n != len(writeToken) {
		fmt.Printf("FS-VALIDATE: FAIL short write %q: wrote %d want %d\n",
			writeName, n, len(writeToken))
		halt()
	}
	_ = f.Fsync(wEnt.NodeID, wfh, false)

	// Read our own write back through the FUSE path to confirm the write
	// is durable and addressable in-guest before claiming the host check.
	rb, err := f.Read(wEnt.NodeID, wfh, 0, uint32(len(writeToken)))
	if err != nil {
		fmt.Printf("FS-VALIDATE: FAIL Read-back of own write %q: %v\n", writeName, err)
		halt()
	}
	_ = f.Release(wEnt.NodeID, wfh)

	if string(rb) != writeToken {
		fmt.Printf("FS-VALIDATE: FAIL own-write read-back mismatch: got %q want %q\n",
			string(rb), writeToken)
		halt()
	}

	fmt.Printf("FS-VALIDATE: wrote %q nodeid=%d bytes=%d token=%q\n",
		writeName, wEnt.NodeID, n, writeToken)
	fmt.Printf("FS-VALIDATE: HOST-CHECK file=%s expect=%s\n", writeName, writeToken)

	fmt.Printf("FS-VALIDATE: PASS read %q byte-equal (%d B) + wrote %q (%d B, read-back equal)\n",
		readName, len(got), writeName, n)
	halt()
}

func halt() {
	for {
	}
}
