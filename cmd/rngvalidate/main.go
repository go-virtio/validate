// Headless go-virtio rng validate harness: boots tamago/amd64 under QEMU,
// drives a real virtio-rng-pci device through the go-virtio/rng driver, and
// proves the device actually delivers entropy. No host-side observation is
// needed — the guest does the proof itself and reports RNG-VALIDATE: PASS/FAIL
// on the serial console. run-rng.sh parses the verdict and exits non-zero on
// FAIL.
//
// This mirrors the blk harness: a *real* virtio-rng-pci device (not the fake
// transport the unit tests use) exercises the request-queue bring-up, the
// device-writable buffer post, the doorbell, and the used-ring length report
// on actual QEMU emulation.
//
// Entropy proof (three independent checks, any failure => FAIL):
//
//   - LENGTH: a Read of N bytes returns exactly N (io.ReadFull semantics).
//   - DISTINCT: two successive N-byte reads differ. A stuck/zero source
//     returns identical buffers; real entropy does not.
//   - DISTRIBUTION: over a large sample, (a) not all bytes are zero, (b) the
//     number of distinct byte values is high, and (c) a chi-square-style
//     uniformity check over the 256 buckets is within a generous bound. A
//     constant or low-entropy source fails at least one of these.
package main

import (
	"fmt"

	_ "github.com/go-virtio/validate/board"
	"github.com/go-virtio/validate/transport"

	"github.com/usbarmory/tamago/soc/intel/pci"

	"github.com/go-virtio/common"
	"github.com/go-virtio/rng"
)

const (
	// readLen is the size of each entropy read used for the LENGTH and
	// DISTINCT checks.
	readLen = 64

	// sampleLen is the size of the large sample used for the DISTRIBUTION
	// checks. 8 KiB spans two pages, exercising the driver's multi-chunk
	// fill loop, and gives every one of the 256 byte values ~32 expected
	// hits — enough for a meaningful uniformity bound.
	sampleLen = 8192

	// minDistinctValues is the floor on how many of the 256 possible byte
	// values must appear in the sample. A healthy source covers almost all
	// of them; a degenerate source covers very few. 200/256 is a generous
	// floor (real QEMU backends cover ~256).
	minDistinctValues = 200

	// chiSquareMax bounds the chi-square statistic over the 256 buckets.
	// For 255 degrees of freedom the 99.9th percentile is ~330; we use a
	// generous 400 so a healthy source never trips it but a constant /
	// low-entropy source (which produces an enormous statistic) always
	// does.
	chiSquareMax = 400
)

func main() {
	dev := pci.Probe(0, common.PCIVendorID, common.PCIDeviceIDModernEntropy)
	if dev == nil {
		fmt.Printf("RNG-VALIDATE: FAIL no virtio-rng-pci device found\n")
		halt()
	}

	t := transport.New(dev)

	r, err := rng.OpenVirtioRng(t)
	if err != nil {
		fmt.Printf("RNG-VALIDATE: FAIL OpenVirtioRng: %v\n", err)
		halt()
	}

	fmt.Printf("RNG-VALIDATE: RNG=%#04x:%#04x features=%#x\n",
		dev.Vendor, dev.Device, r.NegotiatedFeatures)

	// Check 1: LENGTH — a Read returns exactly the requested count.
	a := make([]byte, readLen)
	n, err := r.Read(a)
	if err != nil {
		fmt.Printf("RNG-VALIDATE: FAIL Read(#1): %v\n", err)
		halt()
	}
	if n != readLen {
		fmt.Printf("RNG-VALIDATE: FAIL Read(#1) length: got %d want %d\n", n, readLen)
		halt()
	}

	// Check 2: DISTINCT — a second Read differs from the first.
	b := make([]byte, readLen)
	n, err = r.Read(b)
	if err != nil {
		fmt.Printf("RNG-VALIDATE: FAIL Read(#2): %v\n", err)
		halt()
	}
	if n != readLen {
		fmt.Printf("RNG-VALIDATE: FAIL Read(#2) length: got %d want %d\n", n, readLen)
		halt()
	}
	if equal(a, b) {
		fmt.Printf("RNG-VALIDATE: FAIL two successive %d-byte reads are byte-identical (stuck source?)\n", readLen)
		halt()
	}

	// Surface the first few bytes of each read so the transcript shows real,
	// differing data rather than a bare verdict.
	fmt.Printf("RNG-VALIDATE: read1[0:8]=%s\n", hex8(a))
	fmt.Printf("RNG-VALIDATE: read2[0:8]=%s\n", hex8(b))

	// Check 3: DISTRIBUTION over a large sample.
	s := make([]byte, sampleLen)
	n, err = r.Read(s)
	if err != nil {
		fmt.Printf("RNG-VALIDATE: FAIL Read(sample): %v\n", err)
		halt()
	}
	if n != sampleLen {
		fmt.Printf("RNG-VALIDATE: FAIL Read(sample) length: got %d want %d\n", n, sampleLen)
		halt()
	}

	var hist [256]int
	allZero := true
	for _, c := range s {
		hist[c]++
		if c != 0 {
			allZero = false
		}
	}
	if allZero {
		fmt.Printf("RNG-VALIDATE: FAIL sample is all-zero (no entropy delivered)\n")
		halt()
	}

	distinct := 0
	for _, h := range hist {
		if h > 0 {
			distinct++
		}
	}
	if distinct < minDistinctValues {
		fmt.Printf("RNG-VALIDATE: FAIL only %d of 256 byte values present (< %d); low-entropy source\n",
			distinct, minDistinctValues)
		halt()
	}

	// Chi-square over 256 buckets: sum((observed-expected)^2/expected).
	// expected = sampleLen/256. Computed in fixed-point to avoid floats.
	expected := sampleLen / 256 // 32 for 8192
	chi := 0
	for _, h := range hist {
		d := h - expected
		chi += (d * d * 100) / expected // *100 keeps one extra digit of precision
	}
	chi /= 100
	if chi > chiSquareMax {
		fmt.Printf("RNG-VALIDATE: FAIL chi-square=%d exceeds %d (non-uniform; suspect source)\n",
			chi, chiSquareMax)
		halt()
	}

	fmt.Printf("RNG-VALIDATE: sample=%d distinct_values=%d chi_square=%d (max %d)\n",
		sampleLen, distinct, chi, chiSquareMax)
	fmt.Printf("RNG-VALIDATE: PASS entropy delivered: length OK, two reads differ, %d/256 values, chi=%d\n",
		distinct, chi)
	halt()
}

// hex8 renders the first up-to-8 bytes of b as lowercase hex.
func hex8(b []byte) string {
	const digits = "0123456789abcdef"
	n := len(b)
	if n > 8 {
		n = 8
	}
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		out = append(out, digits[b[i]>>4], digits[b[i]&0x0f])
	}
	return string(out)
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
