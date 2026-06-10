//go:build vtest_live

// Package vtest live integration test.
//
// This file is gated behind the `vtest_live` build tag because it requires a
// running virgl_test_server (from the virglrenderer pantry package, Deliverable
// 1). That server needs surfaceless EGL + llvmpipe, which is a Linux-only setup
// in this environment (macOS has no native EGL — see the package.yml note). On a
// Linux host:
//
//	pkgx +gitlab.freedesktop.org/virgl/virglrenderer virgl_test_server \
//	    --no-loop-or-fork --use-egl-surfaceless --use-gles &
//	GOWORK=off go test -tags vtest_live ./...
//
// Run with VTEST_SOCKET to point at a non-default socket path.
package vtest

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// buildClearToRedVirglBuffer is a stand-in for go-virtio/gpu's
// buildClearVirglBuffer: it would be replaced by importing the real builder.
// Here we only need *some* well-formed dword-aligned buffer to prove the
// SUBMIT_CMD path end-to-end; the real go-virtio buffer drops in unchanged.
func buildClearToRedVirglBuffer(t *testing.T) []byte {
	t.Helper()
	// Placeholder: a single no-op-ish dword-aligned stream. The real test wires
	// in gpu.buildClearVirglBuffer(width,height, [4]float32{1,0,0,1}).
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	return buf.Bytes()
}

func TestLiveClearReadback(t *testing.T) {
	sock := os.Getenv("VTEST_SOCKET")
	if sock == "" {
		sock = DefaultSocketName
	}

	c, conn, err := Dial(sock)
	if err != nil {
		t.Skipf("no virgl_test_server at %s (Linux-only): %v", sock, err)
	}
	defer conn.Close()

	if _, err := c.Handshake("go-virtio-validate", VtestProtocolVersion); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	const w, h = 4, 4
	const bgra = 1 // VIRGL_FORMAT_B8G8R8A8_UNORM (matches go-virtio/gpu)
	res := ResourceCreateArgs{
		Handle: 1, Target: 2 /*2D*/, Format: bgra, Bind: 2, /*RENDER_TARGET*/
		Width: w, Height: h, Depth: 1, ArraySize: 1,
	}
	if err := c.CreateResource(res); err != nil {
		t.Fatalf("create resource: %v", err)
	}

	if err := c.SubmitCmd(buildClearToRedVirglBuffer(t)); err != nil {
		t.Fatalf("submit cmd: %v", err)
	}

	const stride = w * 4
	pixels, err := c.TransferGet(TransferGetArgs{
		Handle: 1, Stride: stride, LayerStride: stride * h,
		Width: w, Height: h, Depth: 1, DataSize: stride * h,
	})
	if err != nil {
		t.Fatalf("transfer get: %v", err)
	}

	// Assert the first pixel is red in BGRA: B=0,G=0,R=255,A=255.
	if len(pixels) < 4 {
		t.Fatalf("short readback: %d bytes", len(pixels))
	}
	got := pixels[0:4]
	want := []byte{0x00, 0x00, 0xFF, 0xFF}
	if !bytes.Equal(got, want) {
		t.Fatalf("pixel[0] = %v, want red %v", got, want)
	}
}
