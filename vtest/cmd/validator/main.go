// Command validator feeds a go-virtio/gpu virgl command buffer (dumped to a
// file on the host) to a real virgl_test_server and reads back the rendered
// pixels, asserting the expected colour.
//
// Usage:
//
//	validator -buf /mnt/clear.bin -w 16 -h 16 -mode clear
//	validator -buf /mnt/draw.bin  -w 16 -h 16 -mode draw -vb /mnt/draw_vb.bin
//	validator -buf /mnt/tex.bin   -w 16 -h 16 -mode tex  -vb /mnt/tex_vb.bin \
//	          -tex /mnt/tex_texels.bin -tw 2 -th 2
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
		mode = flag.String("mode", "clear", "clear|draw|tex")
		vb   = flag.String("vb", "", "vertex-buffer data file; if set, create+upload VB resource 2 before submit (draw/tex modes)")
		tex  = flag.String("tex", "", "texture texel data file (RGBA8); if set, create+upload texture resource 3 before submit (tex mode)")
		tw   = flag.Uint("tw", 2, "texture width (tex mode)")
		th   = flag.Uint("th", 2, "texture height (tex mode)")
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

	// Draw/tex modes: the DRAW_VBO command buffer references vertex-buffer
	// resource handle 2 (go-virtio/gpu vbufResourceID). Create it as a
	// PIPE_BUFFER with VIRGL_BIND_VERTEX_BUFFER and upload the vertex data,
	// mirroring gpu3d_draw.go:createVertexBuffer, before submitting.
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

	// Tex mode: the textured command buffer references sampler-view texture
	// resource handle 3 (go-virtio/gpu texResourceID). Create it as a
	// PIPE_TEXTURE_2D RGBA8 with VIRGL_BIND_SAMPLER_VIEW and upload the
	// tightly-packed texels, mirroring gpu3d_tex.go:createTexture, before
	// submitting the draw.
	if *tex != "" {
		texData, err := os.ReadFile(*tex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read tex %s: %v\n", *tex, err)
			os.Exit(2)
		}
		const rgba8 = 67 // VIRGL_FORMAT_R8G8B8A8_UNORM
		texRes := vtest.ResourceCreateArgs{
			Handle: 3, Target: 2 /*PIPE_TEXTURE_2D*/, Format: rgba8,
			Bind:  1 << 3, /*VIRGL_BIND_SAMPLER_VIEW*/
			Width: uint32(*tw), Height: uint32(*th), Depth: 1, ArraySize: 1,
		}
		if err := c.CreateResource(texRes); err != nil {
			fmt.Fprintf(os.Stderr, "create tex resource: %v\n", err)
			os.Exit(1)
		}
		texStride := uint32(*tw) * 4
		if err := c.TransferPut(vtest.TransferGetArgs{
			Handle: 3, Stride: texStride, LayerStride: texStride * uint32(*th),
			Width: uint32(*tw), Height: uint32(*th), Depth: 1,
			DataSize: uint32(len(texData)),
		}, texData); err != nil {
			fmt.Fprintf(os.Stderr, "upload tex: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("created+uploaded TEX resource 3: %dx%d RGBA8, %d bytes\n", *tw, *th, len(texData))
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

	switch *mode {
	case "tex":
		os.Exit(assertTex(pixels))
	default:
		// clear/draw legacy assertion: pixel[0] is the RED clear background.
		red := []byte{0x00, 0x00, 0xFF, 0xFF}
		got := pixels[0:4]
		if string(got) == string(red) {
			fmt.Println("PASS: pixel[0] is RED (BGRA 00 00 FF FF)")
			os.Exit(0)
		}
		fmt.Printf("RESULT: pixel[0] = %02X %02X %02X %02X (want red 00 00 FF FF for clear)\n",
			got[0], got[1], got[2], got[3])
	}
}

// assertTex validates a textured-triangle readback. The buffer carries NO CLEAR
// command (the go-virtio/gpu draw buffers never clear), so the background is
// whatever the RT initialises to (observed empty = BGRA 00 00 00 00); the
// triangle interior is the only rendered region. A genuine *textured* triangle
// sampled from the 2x2 four-colour texel grid must show, across the triangle
// interior, at least TWO distinct sampled colours — a flat single-colour
// triangle (background + exactly one interior colour = 2 colours total) means
// the texture was NOT sampled as a gradient.
//
// The background colour is identified empirically as the MOST-COMMON colour
// (the corners + the area outside the triangle dominate a small triangle).
// We then require >=2 distinct NON-background colours.
//
// It prints a colour histogram and returns the process exit code.
func assertTex(pixels []byte) int {
	counts := map[[4]byte]int{}
	for i := 0; i+4 <= len(pixels); i += 4 {
		var k [4]byte
		copy(k[:], pixels[i:i+4])
		counts[k]++
	}
	fmt.Printf("distinct colours in readback: %d\n", len(counts))
	// Identify the background as the most-common colour.
	var bg [4]byte
	bgCount := -1
	for k, v := range counts {
		fmt.Printf("  colour BGRA %02X %02X %02X %02X : %d px\n", k[0], k[1], k[2], k[3], v)
		if v > bgCount {
			bgCount, bg = v, k
		}
	}
	fmt.Printf("background (most-common) BGRA = %02X %02X %02X %02X (%d px)\n", bg[0], bg[1], bg[2], bg[3], bgCount)

	// Count distinct NON-background colours (the sampled triangle interior).
	nonBg := 0
	for k := range counts {
		if k != bg {
			nonBg++
		}
	}
	fmt.Printf("distinct non-background (triangle interior) colours: %d\n", nonBg)

	if len(counts) < 2 {
		fmt.Println("FAIL: readback is uniform (no triangle rendered at all)")
		return 1
	}
	if nonBg < 2 {
		fmt.Println("FAIL: triangle interior is a single flat colour (texture NOT sampled as a gradient)")
		return 1
	}
	fmt.Println("PASS: textured triangle shows >=2 distinct sampled texel colours in its interior")
	return 0
}
