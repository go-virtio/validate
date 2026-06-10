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
	"github.com/go-virtio/validate/vtest/refrender"
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
	case "clear":
		os.Exit(assertClearExact(pixels, int(*w), int(*h)))
	case "draw":
		os.Exit(assertDrawExact(pixels, int(*w), int(*h)))
	case "tex":
		os.Exit(assertTexExact(pixels, int(*w), int(*h)))
	default:
		fmt.Fprintf(os.Stderr, "unknown -mode %q (clear|draw|tex)\n", *mode)
		os.Exit(2)
	}
}

// --- EXACT references for the go-virtio/gpu fixtures ----------------------
//
// These mirror, byte-for-byte, the fixtures the gpu package dumps
// (gpu3d_draw_test.go triVerts/triColor, gpu3d_tex_test.go texTriVerts/texData2)
// and the viewport/sampler state the gpu draw buffers encode. The validator
// builds the SAME render via refrender and asserts the live virglrenderer
// readback matches it.

// gpuVerts is the 3 clip-space vertices (with texcoords) both gpu draw paths
// use: top (0,0.5) uv(0.5,1), bottom-left (-0.5,-0.5) uv(0,0), bottom-right
// (0.5,-0.5) uv(1,0).
var gpuVerts = [3]refrender.Vertex{
	{X: 0.0, Y: 0.5, U: 0.5, V: 1.0},
	{X: -0.5, Y: -0.5, U: 0.0, V: 0.0},
	{X: 0.5, Y: -0.5, U: 1.0, V: 0.0},
}

// gpuFlatColor is gpu3d_draw_test.go triColor (RGBA).
var gpuFlatColor = [4]float32{0.25, 0.5, 0.75, 1.0}

// gpuBG is the render-target's uncleared initial value observed in the readback
// (the gpu draw/tex buffers carry NO CLEAR), BGRA 00 00 00 00.
var gpuBG = refrender.BGRA{0x00, 0x00, 0x00, 0x00}

// edgeTol is the pixel band around the analytic triangle edge within which a
// coverage disagreement is tolerated: rasterizer fill-convention/rounding can
// differ by up to one pixel along an edge. We allow ±1px.
const edgeTol = 1

// drawColourTol is the per-channel tolerance for the flat-draw interior: the
// colour is a single UNORM-packed constant, so the only slack is rounding. ±2.
const drawColourTol = 2

// texColourTol is the per-channel tolerance for the textured interior: llvmpipe
// does fixed-point bilinear blending; our reference does float bilinear. A few
// LSBs of difference are expected. ±4.
const texColourTol = 4

const maxDiffs = 64

// dumpGrids prints the real readback and the reference coverage as side-by-side
// ASCII maps for the serial log.
func dumpGrids(real []byte, refIn []bool, w, h int, bg refrender.BGRA) {
	realIn := refrender.CoverageOf(refrender.BytesToBGRA(real), bg)
	fmt.Println("--- REAL readback coverage (#=non-bg, .=bg) ---")
	fmt.Print(refrender.ASCIIMap(realIn, w, h))
	fmt.Println("--- REFERENCE coverage (#=interior, .=exterior) ---")
	fmt.Print(refrender.ASCIIMap(refIn, w, h))
}

// assertClearExact: every pixel must be RED (BGRA 00 00 FF FF). The clear buffer
// fills the whole RT, so this is an exact full-frame check (no tolerance).
func assertClearExact(pixels []byte, w, h int) int {
	red := refrender.BGRA{0x00, 0x00, 0xFF, 0xFF}
	px := refrender.BytesToBGRA(pixels)
	bad := 0
	var first refrender.PixelDiff
	for i, p := range px {
		if p != red {
			if bad == 0 {
				first = refrender.PixelDiff{X: i % w, Y: i / w, Real: p, Ref: red}
			}
			bad++
		}
	}
	fmt.Printf("CLEAR exact: %d/%d pixels == RED (BGRA 00 00 FF FF)\n", len(px)-bad, len(px))
	if bad == 0 {
		fmt.Println("PASS: CLEAR exact — every pixel is RED")
		return 0
	}
	fmt.Printf("FAIL: CLEAR — %d pixels != RED; first at (%d,%d) real=%02X%02X%02X%02X\n",
		bad, first.X, first.Y, first.Real[0], first.Real[1], first.Real[2], first.Real[3])
	return 1
}

// assertDrawExact: coverage matches the analytic triangle within ±1px and the
// interior is the flat gpu colour within ±2 per channel.
func assertDrawExact(pixels []byte, w, h int) int {
	vp := refrender.ViewportFor(w, h)
	ref, cov := refrender.DrawReference(vp, gpuVerts, gpuFlatColor, gpuBG, w, h)
	dumpGrids(pixels, cov.In, w, h, gpuBG)
	res := refrender.Compare(pixels, ref, cov, gpuBG, edgeTol, drawColourTol, maxDiffs)
	fmt.Print(res.Summary())
	fill := drawFlatBGRA()
	fmt.Printf("expected flat interior BGRA = %02X%02X%02X%02X\n", fill[0], fill[1], fill[2], fill[3])
	if res.Pass {
		fmt.Println("PASS: DRAW exact — coverage matches analytic triangle (±1px) and flat colour within ±2")
		return 0
	}
	fmt.Println("FAIL: DRAW exact — readback diverges from reference (see DIFF lines)")
	return 1
}

// drawFlatBGRA returns the gpu flat colour packed as BGRA (for the report
// line): the first interior pixel of the reference render.
func drawFlatBGRA() refrender.BGRA {
	ref, cov := refrender.DrawReference(refrender.ViewportFor(16, 16), gpuVerts, gpuFlatColor, gpuBG, 16, 16)
	for i, in := range cov.In {
		if in {
			return ref[i]
		}
	}
	return refrender.BGRA{}
}

// assertTexExact: coverage matches the analytic triangle within ±1px and the
// interior colours match the LINEAR-sampled reference within ±4 per channel.
// The gpu sampler state is LINEAR/CLAMP_TO_EDGE (gpu3d_tex.go samplerStateS0),
// so the reference uses the LINEAR filter — matching the gpu's ACTUAL filter.
func assertTexExact(pixels []byte, w, h int) int {
	vp := refrender.ViewportFor(w, h)
	tex := []byte{
		0xFF, 0x00, 0x00, 0xFF, // (0,0) red
		0x00, 0xFF, 0x00, 0xFF, // (1,0) green
		0x00, 0x00, 0xFF, 0xFF, // (0,1) blue
		0xFF, 0xFF, 0xFF, 0xFF, // (1,1) white
	}
	ref, cov := refrender.TexReference(vp, gpuVerts, tex, 2, 2, refrender.Linear, gpuBG, w, h)
	dumpGrids(pixels, cov.In, w, h, gpuBG)
	res := refrender.Compare(pixels, ref, cov, gpuBG, edgeTol, texColourTol, maxDiffs)
	fmt.Print(res.Summary())
	if res.Pass {
		fmt.Println("PASS: TEX exact — coverage matches analytic triangle (±1px) and LINEAR-sampled colours within ±4")
		return 0
	}
	fmt.Println("FAIL: TEX exact — readback diverges from LINEAR reference (see DIFF lines)")
	return 1
}
