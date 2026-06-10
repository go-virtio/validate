// Command mkseed builds a cloud-init NoCloud seed ISO in pure Go (CGO=0, no
// external tool), using github.com/openweft/weft-cidata. It replaces the
// `xorriso -V cidata -o seed.iso ...` invocation in the Debian-VM validation
// flow (the virgl/Venus vtest harness), keeping the whole harness free of
// external C binaries.
//
// Usage:
//
//	go run ./cmd/mkseed -instance-id virgl-validate-009 \
//	    -user-data user-data -out seed.iso
//
// The user-data file is the full cloud-init document (it may embed the
// validator + test buffers via write_files/base64). A meta-data file carrying
// the instance-id is generated automatically by weft-cidata; bump -instance-id
// between runs so cloud-init re-runs runcmd.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	cloudinit "github.com/openweft/weft-cidata"
)

func main() {
	instanceID := flag.String("instance-id", "validate", "cloud-init instance-id (bump to force a re-run)")
	hostname := flag.String("hostname", "debian", "guest hostname")
	userData := flag.String("user-data", "-", "path to the cloud-init user-data file (- for stdin)")
	out := flag.String("out", "seed.iso", "output ISO path")
	flag.Parse()

	ud, err := readUserData(*userData)
	if err != nil {
		fail(err)
	}
	iso, err := cloudinit.BuildCloudInitISO(*instanceID, *hostname, string(ud))
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, iso, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("mkseed: wrote %s (%d bytes, NoCloud label \"cidata\", instance-id %q)\n",
		*out, len(iso), *instanceID)
}

func readUserData(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkseed:", err)
	os.Exit(1)
}
