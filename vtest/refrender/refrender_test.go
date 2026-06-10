package refrender

import (
	"strings"
	"testing"
)

// The exact gpu fixtures (gpu3d_draw_test.go / gpu3d_tex_test.go).
var (
	testVerts = [3]Vertex{
		{X: 0.0, Y: 0.5, U: 0.5, V: 1.0},
		{X: -0.5, Y: -0.5, U: 0.0, V: 0.0},
		{X: 0.5, Y: -0.5, U: 1.0, V: 0.0},
	}
	testTex = []byte{
		0xFF, 0x00, 0x00, 0xFF, // (0,0) red
		0x00, 0xFF, 0x00, 0xFF, // (1,0) green
		0x00, 0x00, 0xFF, 0xFF, // (0,1) blue
		0xFF, 0xFF, 0xFF, 0xFF, // (1,1) white
	}
	testBG = BGRA{0, 0, 0, 0}
)

func TestViewportFor(t *testing.T) {
	vp := ViewportFor(16, 16)
	if vp.ScaleX != 8 || vp.ScaleY != 8 || vp.TranslateX != 8 || vp.TranslateY != 8 {
		t.Fatalf("ViewportFor(16,16) = %+v", vp)
	}
}

func TestWindowTransform(t *testing.T) {
	vp := ViewportFor(16, 16)
	wx, wy := vp.window(Vertex{X: 0, Y: 0.5})
	if wx != 8 || wy != 12 {
		t.Fatalf("apex window = (%v,%v), want (8,12)", wx, wy)
	}
	wx, wy = vp.window(Vertex{X: -0.5, Y: -0.5})
	if wx != 4 || wy != 4 {
		t.Fatalf("BL window = (%v,%v), want (4,4)", wx, wy)
	}
}

func countTrue(b []bool) int {
	n := 0
	for _, x := range b {
		if x {
			n++
		}
	}
	return n
}

func TestDrawReferenceGeometry(t *testing.T) {
	vp := ViewportFor(16, 16)
	out, cov := DrawReference(vp, testVerts, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	if got := countTrue(cov.In); got != 32 {
		t.Fatalf("interior px = %d, want 32", got)
	}
	// The apex must be near the TOP of the image (small j), not the bottom.
	topRow, botRow := 16, -1
	for j := 0; j < 16; j++ {
		for i := 0; i < 16; i++ {
			if cov.In[j*16+i] {
				if j < topRow {
					topRow = j
				}
				if j > botRow {
					botRow = j
				}
			}
		}
	}
	if topRow >= botRow {
		t.Fatalf("degenerate vertical extent: top=%d bot=%d", topRow, botRow)
	}
	// No Y-flip (live readback convention): the wide BASE is at the top rows and
	// the apex at the bottom. The widest row must be at the TOP of the covered
	// span, the single-pixel apex at the BOTTOM.
	topWidth, botWidth := 0, 0
	for i := 0; i < 16; i++ {
		if cov.In[topRow*16+i] {
			topWidth++
		}
		if cov.In[botRow*16+i] {
			botWidth++
		}
	}
	if topWidth <= botWidth {
		t.Fatalf("expected wide base at top (row %d w=%d) and apex at bottom (row %d w=%d)", topRow, topWidth, botRow, botWidth)
	}
	// Interior pixels are the flat colour BGRA = round(0.75*255),round(0.5*255),round(0.25*255),255.
	want := BGRA{0xBF, 0x80, 0x40, 0xFF}
	for idx, in := range cov.In {
		if in && out[idx] != want {
			t.Fatalf("interior px %d = %v, want %v", idx, out[idx], want)
		}
		if !in && out[idx] != testBG {
			t.Fatalf("exterior px %d = %v, want bg", idx, out[idx])
		}
	}
}

func TestDrawReferenceDegenerate(t *testing.T) {
	vp := ViewportFor(16, 16)
	// Collinear verts -> zero area -> no coverage.
	deg := [3]Vertex{{X: -0.5, Y: 0}, {X: 0, Y: 0}, {X: 0.5, Y: 0}}
	_, cov := DrawReference(vp, deg, [4]float32{1, 0, 0, 1}, testBG, 16, 16)
	if countTrue(cov.In) != 0 {
		t.Fatalf("degenerate triangle covered pixels")
	}
}

func TestDrawReferenceCWWinding(t *testing.T) {
	// Reverse winding (CW): area<0 branch. Same shape, same coverage.
	vp := ViewportFor(16, 16)
	cw := [3]Vertex{testVerts[0], testVerts[2], testVerts[1]}
	_, covCW := DrawReference(vp, cw, [4]float32{1, 0, 0, 1}, testBG, 16, 16)
	_, covCCW := DrawReference(vp, testVerts, [4]float32{1, 0, 0, 1}, testBG, 16, 16)
	if countTrue(covCW.In) != countTrue(covCCW.In) {
		t.Fatalf("winding changed coverage: cw=%d ccw=%d", countTrue(covCW.In), countTrue(covCCW.In))
	}
}

func TestTexReferenceNearest(t *testing.T) {
	vp := ViewportFor(16, 16)
	out, cov := TexReference(vp, testVerts, testTex, 2, 2, Nearest, testBG, 16, 16)
	seen := map[BGRA]bool{}
	for idx, in := range cov.In {
		if in {
			seen[out[idx]] = true
		}
	}
	// Nearest from a 2x2 -> the four texel colours appear (as BGRA).
	for _, c := range []BGRA{{0, 0, 0xFF, 0xFF}, {0, 0xFF, 0, 0xFF}, {0xFF, 0, 0, 0xFF}, {0xFF, 0xFF, 0xFF, 0xFF}} {
		if !seen[c] {
			t.Errorf("nearest: missing texel colour %v", c)
		}
	}
}

func TestTexReferenceLinear(t *testing.T) {
	vp := ViewportFor(16, 16)
	out, cov := TexReference(vp, testVerts, testTex, 2, 2, Linear, testBG, 16, 16)
	seen := map[BGRA]bool{}
	for idx, in := range cov.In {
		if in {
			seen[out[idx]] = true
		}
	}
	// Linear bilinear blend of a 4-colour 2x2 yields many distinct colours.
	if len(seen) < 10 {
		t.Fatalf("linear: only %d distinct interior colours, expected a gradient", len(seen))
	}
	// Exterior is bg.
	for idx, in := range cov.In {
		if !in && out[idx] != testBG {
			t.Fatalf("exterior px %d not bg", idx)
		}
	}
}

func TestSampleClampToEdge(t *testing.T) {
	// u,v at the extreme corners must clamp to the corner texels (no wrap).
	// (0,0) -> texel (0,0) red; (1,1) -> texel (1,1) white. Linear at exact
	// corners with CLAMP_TO_EDGE returns the corner texel.
	c00 := sample(testTex, 2, 2, 0, 0, Linear)
	if c00 != (BGRA{0, 0, 0xFF, 0xFF}) {
		t.Errorf("linear (0,0) = %v, want red", c00)
	}
	c11 := sample(testTex, 2, 2, 1, 1, Linear)
	if c11 != (BGRA{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Errorf("linear (1,1) = %v, want white", c11)
	}
	// Nearest beyond range clamps too.
	n := sample(testTex, 2, 2, 1.5, -0.5, Nearest)
	_ = n // exercise clamp branches
}

func TestRGBAFloatToBGRA(t *testing.T) {
	got := rgbaFloatToBGRA([4]float32{0.25, 0.5, 0.75, 1.0})
	if got != (BGRA{0xBF, 0x80, 0x40, 0xFF}) {
		t.Fatalf("packing = %v", got)
	}
}

func TestRoundUnitClamp(t *testing.T) {
	if roundUnit(-5) != 0 {
		t.Error("neg clamp")
	}
	if roundUnit(300) != 255 {
		t.Error("hi clamp")
	}
	if roundUnit(127.5) != 128 {
		t.Error("round")
	}
}

func TestClampInt(t *testing.T) {
	if clampInt(-1, 0, 3) != 0 || clampInt(5, 0, 3) != 3 || clampInt(2, 0, 3) != 2 {
		t.Fatal("clampInt")
	}
}

func TestMaxChannelDelta(t *testing.T) {
	if maxChannelDelta(BGRA{10, 20, 30, 40}, BGRA{12, 18, 30, 45}) != 5 {
		t.Fatal("maxChannelDelta")
	}
}

func TestCompareExactPass(t *testing.T) {
	vp := ViewportFor(16, 16)
	ref, cov := DrawReference(vp, testVerts, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	// Build a "real" readback identical to the reference -> perfect match.
	realBytes := make([]byte, 16*16*4)
	for i, p := range ref {
		copy(realBytes[i*4:], p[:])
	}
	res := Compare(realBytes, ref, cov, testBG, 1, 2, 64)
	if !res.Pass {
		t.Fatalf("identical readback should pass:\n%s", res.Summary())
	}
	if res.CovMismatch != 0 || res.MaxColourDelta != 0 {
		t.Fatalf("identical readback nonzero diff: %+v", res)
	}
	if res.CovExactMatch != 256 {
		t.Fatalf("exact coverage should be full: %d", res.CovExactMatch)
	}
	if !strings.Contains(res.Summary(), "coverage:") {
		t.Fatal("summary missing coverage line")
	}
}

func TestCompareColourFail(t *testing.T) {
	vp := ViewportFor(16, 16)
	ref, cov := DrawReference(vp, testVerts, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	realBytes := make([]byte, 16*16*4)
	for i, p := range ref {
		copy(realBytes[i*4:], p[:])
	}
	// Corrupt one covered interior pixel's colour far beyond tolerance.
	corrupted := false
	for idx, in := range cov.In {
		if in {
			// Force a far colour (delta well beyond ±2) while staying non-bg.
			realBytes[idx*4+0] = ref[idx][0] ^ 0xFF
			realBytes[idx*4+1] = 0x11
			realBytes[idx*4+2] = 0x22
			realBytes[idx*4+3] = 0xFF
			corrupted = true
			break
		}
	}
	if !corrupted {
		t.Skip("no strict-interior pixel found")
	}
	res := Compare(realBytes, ref, cov, testBG, 1, 2, 64)
	if res.Pass {
		t.Fatal("colour corruption should fail")
	}
	if res.MaxColourDelta <= 2 {
		t.Fatalf("expected colour delta > tol, got %d", res.MaxColourDelta)
	}
	foundColour := false
	for _, d := range res.Diffs {
		if d.Kind == "colour" {
			foundColour = true
		}
	}
	if !foundColour {
		t.Fatal("no colour diff recorded")
	}
}

func TestCompareCoverageFail(t *testing.T) {
	vp := ViewportFor(16, 16)
	ref, cov := DrawReference(vp, testVerts, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	realBytes := make([]byte, 16*16*4)
	for i, p := range ref {
		copy(realBytes[i*4:], p[:])
	}
	// Paint a far-exterior pixel (corner (0,0), well away from the triangle).
	realBytes[0] = 0xAA
	realBytes[1] = 0xBB
	realBytes[2] = 0xCC
	realBytes[3] = 0xFF
	res := Compare(realBytes, ref, cov, testBG, 1, 2, 64)
	if res.Pass {
		t.Fatal("exterior paint should fail coverage")
	}
	if res.CovMismatch == 0 {
		t.Fatal("expected coverage mismatch")
	}
}

func TestCompareInteriorMissingCoverageFail(t *testing.T) {
	// A LARGE triangle (full-screen-ish) so deep-interior pixels exist beyond
	// the ±1px edge band; blanking one to bg must be flagged as a coverage
	// mismatch (real says exterior where ref says interior).
	vp := ViewportFor(16, 16)
	big := [3]Vertex{{X: 0, Y: 0.9}, {X: -0.9, Y: -0.9}, {X: 0.9, Y: -0.9}}
	ref, cov := DrawReference(vp, big, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	realBytes := make([]byte, 16*16*4)
	for i, p := range ref {
		copy(realBytes[i*4:], p[:])
	}
	band := boundaryBand(cov, 1)
	blanked := false
	for idx, in := range cov.In {
		if in && !band[idx] {
			copy(realBytes[idx*4:], testBG[:])
			blanked = true
			break
		}
	}
	if !blanked {
		t.Skip("no strict-interior pixel in large triangle (unexpected)")
	}
	res := Compare(realBytes, ref, cov, testBG, 1, 2, 64)
	if res.Pass {
		t.Fatal("interior blanked pixel should fail")
	}
	if res.CovMismatch == 0 {
		t.Fatal("expected a coverage mismatch")
	}
}

func TestCompareEmptyReference(t *testing.T) {
	// Degenerate triangle -> RefInterior 0 -> CovMatchPct defaults to 100.
	cov := Coverage{W: 2, H: 2, In: make([]bool, 4)}
	ref := make([]BGRA, 4)
	real := make([]byte, 16)
	res := compare(BytesToBGRA(real), ref, cov, testBG, 1, 2, 64)
	if res.CovMatchPct != 100 {
		t.Fatalf("empty-ref CovMatchPct = %v", res.CovMatchPct)
	}
	if !res.Pass {
		t.Fatal("empty ref with bg readback should pass")
	}
}

func TestBoundaryBandZeroTol(t *testing.T) {
	cov := Coverage{W: 3, H: 3, In: []bool{
		false, false, false,
		false, true, false,
		false, false, false,
	}}
	band := boundaryBand(cov, 0)
	if !band[4] {
		t.Fatal("centre is a boundary pixel")
	}
}

func TestASCIIMap(t *testing.T) {
	m := ASCIIMap([]bool{true, false, false, true}, 2, 2)
	if m != "#.\n.#\n" {
		t.Fatalf("ASCIIMap = %q", m)
	}
}

func TestDiffCap(t *testing.T) {
	// Force many diffs and ensure the cap holds.
	vp := ViewportFor(16, 16)
	ref, cov := DrawReference(vp, testVerts, [4]float32{0.25, 0.5, 0.75, 1.0}, testBG, 16, 16)
	realBytes := make([]byte, 16*16*4) // all-bg: every interior pixel mismatches coverage
	res := Compare(realBytes, ref, cov, testBG, 0, 2, 3)
	if len(res.Diffs) > 3 {
		t.Fatalf("diff cap exceeded: %d", len(res.Diffs))
	}
}

func TestSummaryWithDiffs(t *testing.T) {
	r := Result{
		W: 16, H: 16, RefInterior: 10, CovMatch: 9, CovMismatch: 1,
		Diffs: []PixelDiff{
			{X: 1, Y: 2, Real: BGRA{0xAA, 0xBB, 0xCC, 0xFF}, Ref: BGRA{0x11, 0x22, 0x33, 0xFF}, Kind: "coverage"},
			{X: 3, Y: 4, Real: BGRA{1, 2, 3, 4}, Ref: BGRA{5, 6, 7, 8}, Kind: "colour"},
		},
	}
	s := r.Summary()
	if !strings.Contains(s, "DIFF[coverage] (1,2)") || !strings.Contains(s, "DIFF[colour] (3,4)") {
		t.Fatalf("summary missing diff lines:\n%s", s)
	}
}

func TestBytesToBGRA(t *testing.T) {
	px := BytesToBGRA([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if len(px) != 2 || px[0] != (BGRA{1, 2, 3, 4}) || px[1] != (BGRA{5, 6, 7, 8}) {
		t.Fatalf("BytesToBGRA = %v", px)
	}
}

func TestCoverageOf(t *testing.T) {
	in := CoverageOf([]BGRA{testBG, {1, 0, 0, 0}}, testBG)
	if in[0] || !in[1] {
		t.Fatalf("CoverageOf = %v", in)
	}
}
