module github.com/go-virtio/validate/vtest

go 1.26.3

// The venus module is consumed only by the test cross-check
// (venuscs_test.go: ring.EncodeCreateInfo). It is a sibling repo in the
// go-virtio org; until a tagged version is wired in CI it is resolved locally.
require github.com/go-virtio/venus v0.3.0
