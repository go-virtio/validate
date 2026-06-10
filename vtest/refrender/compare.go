package refrender

import "fmt"

// PixelDiff is one out-of-tolerance pixel: index (row-major), the real readback
// BGRA, the reference BGRA, and which check it failed.
type PixelDiff struct {
	X, Y      int
	Real, Ref BGRA
	Kind      string // "coverage" or "colour"
}

// Result is the outcome of comparing a real readback against the reference.
type Result struct {
	W, H int

	// Coverage: the analytic interior pixel count, how many of those the real
	// readback also "covered" (non-background), and the symmetric mismatch.
	RefInterior   int
	CovMatch      int     // interior pixels where real agrees with ref (within edge tol)
	CovMismatch   int     // pixels disagreeing beyond the ±1px edge band
	CovMatchPct   float64 // CovMatch / RefInterior * 100 (interior coverage agreement)
	CovExactMatch int     // pixels where real-in == ref-in EXACTLY (no edge tolerance)
	CovExactPct   float64

	// Colour: max per-channel absolute delta over strict-interior pixels (those
	// not adjacent to a triangle edge, where rasteriser rounding can't bite).
	MaxColourDelta int
	InteriorTested int

	Diffs []PixelDiff // out-of-tolerance pixels (capped)
	Pass  bool
}

// background returns true if p equals bg.
func isBG(p, bg BGRA) bool { return p == bg }

// CompareDraw / CompareTex share this core. edgeTol is the pixel band (in
// pixels) around the analytic triangle edge within which a coverage
// disagreement is tolerated (rasterizer fill-convention / rounding). colourTol
// is the max allowed per-channel delta for STRICT-interior pixels. bg is the
// background colour to recognise in the real readback.
//
// real and ref are both W*H BGRA, row-major (row 0 top).
func compare(real, ref []BGRA, cov Coverage, bg BGRA, edgeTol int, colourTol int, maxDiffs int) Result {
	w, h := cov.W, cov.H
	res := Result{W: w, H: h}

	// Distance (in pixels) from each pixel to the nearest coverage boundary,
	// so we know which interior pixels are "strict" (>edgeTol from any edge).
	near := boundaryBand(cov, edgeTol)

	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			idx := j*w + i
			refIn := cov.In[idx]
			realIn := !isBG(real[idx], bg)

			if refIn {
				res.RefInterior++
			}

			// Exact coverage agreement (no tolerance).
			if refIn == realIn {
				res.CovExactMatch++
			}

			// Tolerant coverage agreement: a disagreement is forgiven if the
			// pixel sits within edgeTol of the analytic boundary.
			covOK := refIn == realIn || near[idx]
			if refIn {
				if covOK {
					res.CovMatch++
				} else {
					res.CovMismatch++
					res.appendDiff(maxDiffs, PixelDiff{X: i, Y: j, Real: real[idx], Ref: ref[idx], Kind: "coverage"})
				}
			} else if !covOK {
				// Real painted a pixel the reference says is exterior, beyond
				// the edge band: a real coverage discrepancy.
				res.CovMismatch++
				res.appendDiff(maxDiffs, PixelDiff{X: i, Y: j, Real: real[idx], Ref: ref[idx], Kind: "coverage"})
			}

			// Colour check on every COVERED interior pixel (interior in the
			// reference AND non-background in the real readback). llvmpipe does
			// no MSAA, so a covered pixel carries the full sampled colour even
			// on the edge; the per-pixel sample point matches the reference, so
			// the colour must agree within tolerance regardless of edge band.
			// (Edge-band slack governs COVERAGE, not colour.)
			if refIn && realIn {
				res.InteriorTested++
				d := maxChannelDelta(real[idx], ref[idx])
				if d > res.MaxColourDelta {
					res.MaxColourDelta = d
				}
				if d > colourTol {
					res.appendDiff(maxDiffs, PixelDiff{X: i, Y: j, Real: real[idx], Ref: ref[idx], Kind: "colour"})
				}
			}
		}
	}

	if res.RefInterior > 0 {
		res.CovMatchPct = float64(res.CovMatch) / float64(res.RefInterior) * 100
	} else {
		res.CovMatchPct = 100
	}
	res.CovExactPct = float64(res.CovExactMatch) / float64(w*h) * 100

	// Pass = no coverage mismatches beyond the edge band AND max interior colour
	// delta within tolerance.
	res.Pass = res.CovMismatch == 0 && res.MaxColourDelta <= colourTol
	return res
}

func (r *Result) appendDiff(max int, d PixelDiff) {
	if len(r.Diffs) < max {
		r.Diffs = append(r.Diffs, d)
	}
}

// boundaryBand marks every pixel within `tol` (Chebyshev distance) of a
// coverage boundary (a pixel whose in/out differs from a neighbour).
func boundaryBand(cov Coverage, tol int) []bool {
	w, h := cov.W, cov.H
	edge := make([]bool, w*h)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			idx := j*w + i
			in := cov.In[idx]
			// boundary if any 4-neighbour (or the image border) differs.
			b := false
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				ni, nj := i+d[0], j+d[1]
				if ni < 0 || nj < 0 || ni >= w || nj >= h {
					continue
				}
				if cov.In[nj*w+ni] != in {
					b = true
					break
				}
			}
			edge[idx] = b
		}
	}
	if tol <= 0 {
		return edge
	}
	// Dilate the edge set by `tol` (Chebyshev).
	band := make([]bool, w*h)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			if !edge[j*w+i] {
				continue
			}
			for dj := -tol; dj <= tol; dj++ {
				for di := -tol; di <= tol; di++ {
					ni, nj := i+di, j+dj
					if ni < 0 || nj < 0 || ni >= w || nj >= h {
						continue
					}
					band[nj*w+ni] = true
				}
			}
		}
	}
	return band
}

// maxChannelDelta is the max absolute per-channel difference between two BGRA.
func maxChannelDelta(a, b BGRA) int {
	m := 0
	for k := 0; k < 4; k++ {
		d := int(a[k]) - int(b[k])
		if d < 0 {
			d = -d
		}
		if d > m {
			m = d
		}
	}
	return m
}

// ASCIIMap renders a coverage / readback grid as a 16x16 (or WxH) glyph map:
// '#' interior, '.' exterior — useful for eyeballing the serial log.
func ASCIIMap(in []bool, w, h int) string {
	out := make([]byte, 0, (w+1)*h)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			if in[j*w+i] {
				out = append(out, '#')
			} else {
				out = append(out, '.')
			}
		}
		out = append(out, '\n')
	}
	return string(out)
}

// CoverageOf returns a boolean grid of non-background pixels in a readback.
func CoverageOf(px []BGRA, bg BGRA) []bool {
	in := make([]bool, len(px))
	for i, p := range px {
		in[i] = !isBG(p, bg)
	}
	return in
}

// Summary formats the Result as a human-readable multi-line report.
func (r Result) Summary() string {
	s := fmt.Sprintf("coverage: ref-interior=%d px, tolerant-match=%d (%.1f%%), exact-match=%d/%d (%.1f%%), mismatch=%d\n",
		r.RefInterior, r.CovMatch, r.CovMatchPct, r.CovExactMatch, r.W*r.H, r.CovExactPct, r.CovMismatch)
	s += fmt.Sprintf("colour: strict-interior pixels tested=%d, max per-channel delta=%d\n",
		r.InteriorTested, r.MaxColourDelta)
	for _, d := range r.Diffs {
		s += fmt.Sprintf("  DIFF[%s] (%d,%d) real=%02X%02X%02X%02X ref=%02X%02X%02X%02X\n",
			d.Kind, d.X, d.Y, d.Real[0], d.Real[1], d.Real[2], d.Real[3],
			d.Ref[0], d.Ref[1], d.Ref[2], d.Ref[3])
	}
	return s
}

// BytesToBGRA reinterprets a flat BGRA byte buffer (len = w*h*4) as []BGRA.
func BytesToBGRA(b []byte) []BGRA {
	out := make([]BGRA, len(b)/4)
	for i := range out {
		copy(out[i][:], b[i*4:i*4+4])
	}
	return out
}

// Compare is the public entry: given the real readback bytes and a reference
// []BGRA + its coverage, run the comparison with the given tolerances.
func Compare(realBytes []byte, ref []BGRA, cov Coverage, bg BGRA, edgeTol, colourTol, maxDiffs int) Result {
	return compare(BytesToBGRA(realBytes), ref, cov, bg, edgeTol, colourTol, maxDiffs)
}
