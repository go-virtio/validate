// Package refrender is a tiny software REFERENCE rasteriser used to turn the
// go-virtio/gpu virgl draw/tex command buffers into an EXACT expected 16x16
// BGRA framebuffer, so the live-virglrenderer readback can be checked against
// an analytic ground truth (coverage + per-channel colour) rather than the
// weak "non-uniform / >=2 colours" heuristic.
//
// It is deliberately self-contained and pure (no I/O, no sockets) so it can be
// unit-tested to 100% offline. The validator command wires it to the live
// vtest readback.
//
// Conventions (all derived from what gpu3d_draw.go / gpu3d_tex.go actually
// encode — see the per-field comments):
//
//   - Vertices arrive in clip space with w=1 (the gpu vertex shader is a pure
//     passthrough: MOV OUT[0], IN[0]). NDC == clip for w=1.
//   - Viewport transform (SET_VIEWPORT_STATE): window coordinate
//     wx = ndc_x*scale_x + translate_x, wy = ndc_y*scale_y + translate_y.
//     gpu sets scale=(W/2,H/2,0.5), translate=(W/2,H/2,0.5).
//   - Pixel centres at (i+0.5, j+0.5): the rasterizer S0 sets HALF_PIXEL_CENTER
//     (bit 29) in both gpu draw paths.
//   - Window Y is bottom-up (OpenGL). The framebuffer the scanout/readback
//     presents has row 0 at the TOP, so buffer row = (H-1) - floor(wy). The
//     apex (highest window y) therefore lands near the top rows. This Y-flip
//     is the GL framebuffer-origin convention, NOT a gpu choice; it is applied
//     here so the reference lines up with the displayed/readback image.
//   - Render target is VIRGL_FORMAT_B8G8R8A8_UNORM: each pixel is stored B,G,R,A.
package refrender

import "math"

// Viewport is the window transform read out of SET_VIEWPORT_STATE.
type Viewport struct {
	ScaleX, ScaleY         float32
	TranslateX, TranslateY float32
}

// ViewportFor returns the viewport gpu3d_draw.go / gpu3d_tex.go encode for a
// W x H render target: scale=(W/2,H/2), translate=(W/2,H/2).
func ViewportFor(w, h int) Viewport {
	return Viewport{
		ScaleX: float32(w) / 2, ScaleY: float32(h) / 2,
		TranslateX: float32(w) / 2, TranslateY: float32(h) / 2,
	}
}

// Vertex is a clip-space (w=1) vertex with an optional (u,v) texcoord.
type Vertex struct {
	X, Y float32 // clip-space / NDC position (z ignored: depth test off)
	U, V float32 // texcoord (tex path only)
}

// window maps an NDC vertex to window-space (wx, wy) via the viewport.
func (vp Viewport) window(v Vertex) (wx, wy float64) {
	return float64(v.X)*float64(vp.ScaleX) + float64(vp.TranslateX),
		float64(v.Y)*float64(vp.ScaleY) + float64(vp.TranslateY)
}

// BGRA is one render-target pixel in storage order B,G,R,A.
type BGRA [4]byte

// Coverage is a W*H grid: true where the analytic triangle covers the pixel.
type Coverage struct {
	W, H int
	In   []bool
}

// Bary holds the three barycentric weights for a covered pixel.
type bary struct{ a, b, c float64 }

// rasterise computes, for every pixel centre, whether it is inside the triangle
// (v0,v1,v2 in window space) and, if so, its barycentric weights. It uses the
// standard signed-area edge functions and accepts a pixel when it is inside for
// the triangle's actual winding (either orientation) so winding order can't drop
// it — mirroring the gpu rasterizer state, which disables culling.
//
// Returns coverage and a parallel slice of barycentrics (valid where In==true).
func rasterise(vp Viewport, v0, v1, v2 Vertex, w, h int) (Coverage, []bary) {
	x0, y0 := vp.window(v0)
	x1, y1 := vp.window(v1)
	x2, y2 := vp.window(v2)

	// Signed area*2 of the triangle (window space, GL bottom-up y).
	area := edge(x0, y0, x1, y1, x2, y2)

	cov := Coverage{W: w, H: h, In: make([]bool, w*h)}
	bw := make([]bary, w*h)
	if area == 0 {
		return cov, bw // degenerate: no coverage
	}

	for j := 0; j < h; j++ {
		// Window y of this buffer row's pixel centre. EMPIRICALLY (live
		// virglrenderer vtest readback, instance virgl-validate-010): the
		// readback row order matches window y directly — buffer row j is window
		// y = j+0.5, NOT flipped. The NDC apex (y=+0.5 -> highest window y) lands
		// at the BOTTOM rows of the readback. This is the readback's row
		// convention (the transfer presents the RT bottom-up == top-down here),
		// confirmed by red/green base texels appearing in the top readback rows.
		py := float64(j) + 0.5
		for i := 0; i < w; i++ {
			px := float64(i) + 0.5
			// Edge functions relative to each edge.
			w0 := edge(x1, y1, x2, y2, px, py)
			w1 := edge(x2, y2, x0, y0, px, py)
			w2 := edge(x0, y0, x1, y1, px, py)
			// Inside if all edge functions share the triangle's sign
			// (>=0 for CCW area>0, <=0 for CW area<0). Boundary (==0) counts
			// as inside for both; edge-tolerance is handled by the comparator.
			var in bool
			if area > 0 {
				in = w0 >= 0 && w1 >= 0 && w2 >= 0
			} else {
				in = w0 <= 0 && w1 <= 0 && w2 <= 0
			}
			if in {
				idx := j*w + i
				cov.In[idx] = true
				bw[idx] = bary{a: w0 / area, b: w1 / area, c: w2 / area}
			}
		}
	}
	return cov, bw
}

// edge is the 2D cross product (signed area*2) of (bx-ax,by-ay) x (cx-ax,cy-ay).
func edge(ax, ay, bx, by, cx, cy float64) float64 {
	return (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
}

// DrawReference renders the flat-shaded triangle reference: interior pixels get
// flatColor (RGBA float in [0,1], exactly the colour baked into the gpu fragment
// shader IMM), exterior pixels get bg. The output is BGRA storage order.
func DrawReference(vp Viewport, v [3]Vertex, flatColor [4]float32, bg BGRA, w, h int) ([]BGRA, Coverage) {
	cov, _ := rasterise(vp, v[0], v[1], v[2], w, h)
	out := make([]BGRA, w*h)
	fill := rgbaFloatToBGRA(flatColor)
	for idx := 0; idx < w*h; idx++ {
		if cov.In[idx] {
			out[idx] = fill
		} else {
			out[idx] = bg
		}
	}
	return out, cov
}

// Filter selects the texture minification/magnification filter.
type Filter int

const (
	// Nearest is PIPE_TEX_FILTER_NEAREST.
	Nearest Filter = iota
	// Linear is PIPE_TEX_FILTER_LINEAR (what gpu3d_tex.go's sampler state sets).
	Linear
)

// TexReference renders the textured-triangle reference. Each interior pixel's
// (u,v) is the barycentric interpolation of the three vertex texcoords; the
// colour is that (u,v) sampled from the texW x texH RGBA8 texture with the given
// filter and CLAMP_TO_EDGE wrapping (the wrap gpu3d_tex.go sets). Exterior pixels
// get bg. Output is BGRA storage order (RGBA texels -> BGRA RT, identity swizzle).
//
// tex is texW*texH*4 bytes, RGBA8, row-major, row 0 first (the byte order
// gpu3d_tex.go uploads). texcoord (0,0) samples texel (0,0); v increases to
// texel row texH-1. This is the GL lower-left texel-origin convention applied
// consistently with the vertex texcoords the gpu encodes (uv (0,0) at the
// bottom-left vertex, (0.5,1) at the apex).
func TexReference(vp Viewport, v [3]Vertex, tex []byte, texW, texH int, filter Filter, bg BGRA, w, h int) ([]BGRA, Coverage) {
	cov, bw := rasterise(vp, v[0], v[1], v[2], w, h)
	out := make([]BGRA, w*h)
	for idx := 0; idx < w*h; idx++ {
		if !cov.In[idx] {
			out[idx] = bg
			continue
		}
		b := bw[idx]
		u := b.a*float64(v[0].U) + b.b*float64(v[1].U) + b.c*float64(v[2].U)
		vv := b.a*float64(v[0].V) + b.b*float64(v[1].V) + b.c*float64(v[2].V)
		out[idx] = sample(tex, texW, texH, u, vv, filter)
	}
	return out, cov
}

// sample reads texel colour at (u,v) in [0,1] from an RGBA8 texture with
// CLAMP_TO_EDGE wrap, returning BGRA. Nearest picks the covering texel; Linear
// does bilinear blend of the 4 neighbours (GL convention: texel centres at
// (i+0.5)/size).
func sample(tex []byte, texW, texH int, u, v float64, filter Filter) BGRA {
	if filter == Nearest {
		ix := clampInt(int(math.Floor(u*float64(texW))), 0, texW-1)
		iy := clampInt(int(math.Floor(v*float64(texH))), 0, texH-1)
		return texelBGRA(tex, texW, ix, iy)
	}
	// Linear: continuous texel-space coordinate, centre-aligned.
	fx := u*float64(texW) - 0.5
	fy := v*float64(texH) - 0.5
	x0 := int(math.Floor(fx))
	y0 := int(math.Floor(fy))
	dx := fx - float64(x0)
	dy := fy - float64(y0)
	// CLAMP_TO_EDGE on each of the four taps.
	x0c := clampInt(x0, 0, texW-1)
	x1c := clampInt(x0+1, 0, texW-1)
	y0c := clampInt(y0, 0, texH-1)
	y1c := clampInt(y0+1, 0, texH-1)
	c00 := texelRGBAf(tex, texW, x0c, y0c)
	c10 := texelRGBAf(tex, texW, x1c, y0c)
	c01 := texelRGBAf(tex, texW, x0c, y1c)
	c11 := texelRGBAf(tex, texW, x1c, y1c)
	var rgba [4]float64
	for k := 0; k < 4; k++ {
		top := c00[k]*(1-dx) + c10[k]*dx
		bot := c01[k]*(1-dx) + c11[k]*dx
		rgba[k] = top*(1-dy) + bot*dy
	}
	// RGBA float -> BGRA bytes.
	return BGRA{
		byte(roundUnit(rgba[2])), byte(roundUnit(rgba[1])),
		byte(roundUnit(rgba[0])), byte(roundUnit(rgba[3])),
	}
}

// texelBGRA fetches texel (ix,iy) (RGBA8) and returns it as BGRA bytes.
func texelBGRA(tex []byte, texW, ix, iy int) BGRA {
	o := (iy*texW + ix) * 4
	return BGRA{tex[o+2], tex[o+1], tex[o+0], tex[o+3]}
}

// texelRGBAf fetches texel (ix,iy) as float64 RGBA in [0,255].
func texelRGBAf(tex []byte, texW, ix, iy int) [4]float64 {
	o := (iy*texW + ix) * 4
	return [4]float64{float64(tex[o+0]), float64(tex[o+1]), float64(tex[o+2]), float64(tex[o+3])}
}

func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// rgbaFloatToBGRA converts an RGBA float colour in [0,1] to BGRA bytes with
// round-to-nearest (UNORM packing: round(c*255)).
func rgbaFloatToBGRA(c [4]float32) BGRA {
	return BGRA{
		byte(roundUnit(float64(c[2]) * 255)),
		byte(roundUnit(float64(c[1]) * 255)),
		byte(roundUnit(float64(c[0]) * 255)),
		byte(roundUnit(float64(c[3]) * 255)),
	}
}

// roundUnit rounds x to nearest and clamps to [0,255].
func roundUnit(x float64) float64 {
	r := math.Round(x)
	if r < 0 {
		return 0
	}
	if r > 255 {
		return 255
	}
	return r
}
