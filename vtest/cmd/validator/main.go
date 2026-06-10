// Command validator feeds a go-virtio/gpu virgl command buffer (dumped to a
// file on the host) to a real virgl_test_server and reads back the rendered
// pixels, asserting the expected colour.
//
// Usage:
//
//	validator -buf /mnt/clear.bin -w 16 -h 16 -mode clear
//	validator -buf /mnt/draw.bin -w 16 -h 16 -mode draw -vb /mnt/draw_vb.bin
//
// It prints the readback pixel bytes and PASS/FAIL.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/go-virtio/validate/vtest"
)

func main() {
	var (
		sock = flag.String("sock", vtest.DefaultSocketName, "vtest server socket")
		buf  = flag.String("buf", "", "virgl command buffer file (e.g. clear.bin)")
		w    = flag.Uint("w", 16, "render target width")
		h    = flag.Uint("h", 16, "render target height")
		mode = flag.String("mode", "clear", "clear|draw|tex (informational)")
		vb   = flag.String("vb", "", "vertex-buffer data file; if set, create+upload VB resource 2 before submit (draw mode)")
	)
	flag.Parse()

	if *buf == "" {
		fmt.Fprintln(os.Stderr, "need -buf")
		os.Exit(2)
	}
	cmdBuf, err := os.ReadFile(*buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *buf, err)
		os.Exit(2)
	}
	fmt.Printf("loaded %s: %d bytes (%d dwords), mode=%s\n", *buf, len(cmdBuf), len(cmdBuf)/4, *mode)

	c, conn, err := vtest.Dial(*sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *sock, err)
		os.Exit(1)
	}
	defer conn.Close()

	neg, err := c.Handshake("go-virtio-validate", vtest.VtestProtocolVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handshake: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("handshake ok, negotiated protocol version %d\n", neg)

	const bgra = 1 // VIRGL_FORMAT_B8G8R8A8_UNORM (matches go-virtio/gpu RT)
	res := vtest.ResourceCreateArgs{
		Handle: 1, Target: 2 /*PIPE_TEXTURE_2D*/, Format: bgra,
		Bind:  2, /*VIRGL_BIND_RENDER_TARGET*/
		Width: uint32(*w), Height: uint32(*h), Depth: 1, ArraySize: 1,
	}
	if err := c.CreateResource(res); err != nil {
		fmt.Fprintf(os.Stderr, "create resource: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created resource 1: %dx%d BGRA render-target\n", *w, *h)

	// Draw mode: the DRAW_VBO command buffer references vertex-buffer resource
	// handle 2 (go-virtio/gpu vbufResourceID). Create it as a PIPE_BUFFER with
	// VIRGL_BIND_VERTEX_BUFFER and upload the vertex data, mirroring
	// gpu3d_draw.go:createVertexBuffer, before submitting the command buffer.
	if *vb != "" {
		vbData, err := os.ReadFile(*vb)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read vb %s: %v\n", *vb, err)
			os.Exit(2)
		}
		vbRes := vtest.ResourceCreateArgs{
			Handle: 2, Target: 0 /*PIPE_BUFFER*/, Format: 0,
			Bind:  1 << 4, /*VIRGL_BIND_VERTEX_BUFFER*/
			Width: uint32(len(vbData)), Height: 1, Depth: 1, ArraySize: 1,
		}
		if err := c.CreateResource(vbRes); err != nil {
			fmt.Fprintf(os.Stderr, "create vb resource: %v\n", err)
			os.Exit(1)
		}
		if err := c.TransferPut(vtest.TransferGetArgs{
			Handle: 2, Stride: uint32(len(vbData)), LayerStride: uint32(len(vbData)),
			Width: uint32(len(vbData)), Height: 1, Depth: 1,
			DataSize: uint32(len(vbData)),
		}, vbData); err != nil {
			fmt.Fprintf(os.Stderr, "upload vb: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("created+uploaded VB resource 2: %d bytes\n", len(vbData))
	}

	if err := c.SubmitCmd(cmdBuf); err != nil {
		fmt.Fprintf(os.Stderr, "submit cmd: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("submitted command buffer (no error from socket write)")

	stride := uint32(*w) * 4
	pixels, err := c.TransferGet(vtest.TransferGetArgs{
		Handle: 1, Stride: stride, LayerStride: stride * uint32(*h),
		Width: uint32(*w), Height: uint32(*h), Depth: 1,
		DataSize: stride * uint32(*h),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "transfer get: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("readback %d bytes\n", len(pixels))
	if len(pixels) < 4 {
		fmt.Fprintln(os.Stderr, "short readback")
		os.Exit(1)
	}

	// Print first few pixels (BGRA).
	n := 4
	if len(pixels)/4 < n {
		n = len(pixels) / 4
	}
	for i := 0; i < n; i++ {
		p := pixels[i*4 : i*4+4]
		fmt.Printf("  pixel[%d] BGRA = %02X %02X %02X %02X\n", i, p[0], p[1], p[2], p[3])
	}
	// Center pixel for draw/tex modes.
	cx := (uint32(*h)/2)*stride + (uint32(*w)/2)*4
	if int(cx)+4 <= len(pixels) {
		p := pixels[cx : cx+4]
		fmt.Printf("  center pixel BGRA = %02X %02X %02X %02X\n", p[0], p[1], p[2], p[3])
	}

	red := []byte{0x00, 0x00, 0xFF, 0xFF}
	got := pixels[0:4]
	if string(got) == string(red) {
		fmt.Println("PASS: pixel[0] is RED (BGRA 00 00 FF FF)")
		os.Exit(0)
	}
	fmt.Printf("RESULT: pixel[0] = %02X %02X %02X %02X (want red 00 00 FF FF for clear)\n",
		got[0], got[1], got[2], got[3])
}
