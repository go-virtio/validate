module github.com/go-virtio/validate/vtest

go 1.27.1

// The venus module supplies two things consumed here:
//   - the test cross-check (venuscs_test.go: ring.EncodeCreateInfo), and
//   - the full clear-image command closure (package proof): the generated,
//     offline-byte-verified vkCreate*/vkCmd*/vkQueueSubmit encoders + reply
//     decoders the live Venus clear-image driver (venusclear.go) drives over
//     the ring. v0.4.0 is the tag that closes that clear-image -> readback set.
// It is a sibling repo in the go-virtio org; until a tagged version is wired
// into CI it is resolved from the local checkout via the replace below.
require github.com/go-virtio/venus v0.7.0
