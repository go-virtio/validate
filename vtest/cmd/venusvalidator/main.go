//go:build linux

// Command venusvalidator drives a real `virgl_test_server --venus` over the
// vtest socket and attempts the genuine Venus round-trip: bring up a Venus
// context, create + mmap the ring's backing HOST3D blob, register the ring with
// vkCreateRingMESA, write a vkCreateInstance command stream into the ring, and
// busy-poll the HOST-owned head word. A head advance is the host actually
// consuming our ring bytes — the real "host responded" proof.
//
// It prints every host-observable value (negotiated protocol version, timeline
// count, capset length, blob res_id, ring tail/head/status) and PASS/FAIL so
// the serial log records either the confirmed round-trip or the precise wall.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-virtio/validate/vtest"
)

func main() {
	var (
		sock    = flag.String("sock", vtest.DefaultSocketName, "vtest server socket")
		bufSize = flag.Int("buf", 1<<18, "ring command-buffer size (power of two)")
		extra   = flag.Int("extra", 0, "ring extra (reply) region size")
		timeout = flag.Duration("timeout", 3*time.Second, "head-advance poll timeout")
	)
	flag.Parse()

	fmt.Printf("VENUS round-trip: sock=%s bufSize=%d extra=%d timeout=%s\n",
		*sock, *bufSize, *extra, *timeout)

	res, err := vtest.VenusRingRoundTrip(*sock, *bufSize, *extra, *timeout)
	if res != nil {
		fmt.Printf("  negotiated protocol version = %d\n", res.NegotiatedVersion)
		fmt.Printf("  max_timeline_count          = %d\n", res.MaxTimelineCount)
		fmt.Printf("  venus capset length         = %d bytes\n", res.CapsetLen)
		fmt.Printf("  ring blob res_id            = %d\n", res.BlobResID)
		fmt.Printf("  ring tail after write       = %d\n", res.RingTailAfter)
		fmt.Printf("  ring head start             = %d\n", res.HeadStart)
		fmt.Printf("  ring head after poll        = %d\n", res.HeadAfter)
		fmt.Printf("  ring status after poll      = %#x\n", res.StatusAfter)
		fmt.Printf("  HEAD ADVANCED               = %v\n", res.HeadAdvanced)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "VENUS-WALL: %v\n", err)
		os.Exit(1)
	}
	if res.HeadAdvanced {
		fmt.Println("PASS: VENUS round-trip — host advanced the ring head (consumed our vkCreateInstance)")
		os.Exit(0)
	}
	fmt.Println("FAIL: VENUS no round-trip — host did NOT advance head within timeout (ring registered, but consumer did not run)")
	os.Exit(2)
}
