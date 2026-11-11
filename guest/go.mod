// Guest harnesses: bare-metal programs built with the tamago toolchain
// (GOOS=tamago). Kept as its own module so a host `go mod tidy` of the
// repository root never sees the github.com/usbarmory/tamago imports, which
// only compile for tamago's GOOS, and so never strips the go.sum entries the
// guest build needs (go-virtio/validate#6). Maintain this module's go.sum
// with a tamago toolchain: GOOS=tamago GOARCH=amd64 GOOSPKG=github.com/usbarmory/tamago go mod tidy
module github.com/go-virtio/validate/guest

go 1.27.1

require (
	github.com/go-virtio/balloon v0.3.0
	github.com/go-virtio/blk v0.4.0
	github.com/go-virtio/common v0.3.0
	github.com/go-virtio/console v0.3.0
	github.com/go-virtio/fs v0.4.0
	github.com/go-virtio/gpu v0.10.0
	github.com/go-virtio/net v0.3.0
	github.com/go-virtio/rng v0.3.0
	github.com/go-virtio/vsock v0.3.0
	github.com/usbarmory/tamago v1.27.4
)
