package main

import (
	"math"
	"testing"

	mgl "github.com/go-gl/mathgl/mgl32"
)

// The model draw's per-frame garbage (model.go).
//
// WHY THIS EXISTS. model.draw runs once per stage layer, and every skinned
// mesh node used to compute its skin's joint matrices into a new slice and
// upload them. A skin has one texture, so each upload replaced the one before
// and only the last node's matrices were ever drawn. A stage with 56 skinned
// meshes on two skins made 170 texture uploads and ~600 KB of garbage a frame:
// 75 Go GCs in 30 s of fight in the browser build. Now only that last node
// computes them (Scene.skinWritersOf) and an upload happens only when a value
// changed (calculateSkinMatrices). These tests hold every skin texture to what
// the old path left in it, draw by draw, and count the uploads.

// skinTestTexture records SetPixelData uploads.
type skinTestTexture struct {
	uploads int
	data    []float32
}

func (t *skinTestTexture) SetData(data []byte)                                         {}
func (t *skinTestTexture) SetSubData(data []byte, x, y, width, height, stride int32)   {}
func (t *skinTestTexture) SetDataG(data []byte, mag, min, ws, wt TextureSamplingParam) {}
func (t *skinTestTexture) SetPixelData(data []float32) {
	t.uploads++
	t.data = append(t.data[:0], data...)
}
func (t *skinTestTexture) IsValid() bool         { return true }
func (t *skinTestTexture) GetWidth() int32       { return 4 }
func (t *skinTestTexture) GetHeight() int32      { return 4 }
func (t *skinTestTexture) CopyData(src *Texture) {}

func skinTestU32(v uint32) *uint32 { return &v }

func skinTestNode(tr [3]float32, rot [4]float32, children ...uint32) *Node {
	return &Node{visible: true, transformChanged: true, childrenIndex: children,
		transition:         GLTFAnimatableProperty{restValue: tr},
		rotation:           GLTFAnimatableProperty{restValue: rot},
		scale:              GLTFAnimatableProperty{restValue: [3]float32{1, 1, 1}},
		morphTargetWeights: GLTFAnimatableProperty{restValue: []float32{}}}
}

// skinTestModel: two roots, so the per-root order of calculateWorldTransform
// and calculateAnimationData in model.draw is covered.
//
//	root 0 (node 0): joint 1; mesh 2 (skin 0); mesh 3 (skin 1); mesh 9 (skin 2)
//	root 1 (node 4): joints 5, 6; mesh 7 (skin 0); mesh 8 (skin 1)
//
// Skin 0's last writer is node 7 and skin 1's node 8, both after nodes 2 and 3
// that wrote first with other world transforms. Skin 2 is written only from
// root 0 but its joint lives in root 1, so on the first draw it reads a joint
// whose world transform root 1 has not computed yet (the old path did too).
func skinTestModel() *Model {
	q := func(deg float32) [4]float32 { // a rotation about z
		s, c := math.Sincos(float64(deg) * math.Pi / 360)
		return [4]float32{0, 0, float32(s), float32(c)}
	}
	n := []*Node{
		skinTestNode([3]float32{0, 1, 0}, q(0), 1, 2, 3, 9),
		skinTestNode([3]float32{1, 2, 3}, q(30)),
		skinTestNode([3]float32{0.5, 0, 0}, q(0)),
		skinTestNode([3]float32{0, 0.25, 0}, q(10)),
		skinTestNode([3]float32{-2, 0, 1}, q(0), 5, 6, 7, 8),
		skinTestNode([3]float32{0, 3, 0}, q(45)),
		skinTestNode([3]float32{1, 0, -1}, q(-20)),
		skinTestNode([3]float32{0.1, 0.2, 0.3}, q(5)),
		skinTestNode([3]float32{0, 0, 0}, q(0)),
		skinTestNode([3]float32{2, 0, 0}, q(90)),
	}
	for i, s := range map[int]uint32{2: 0, 7: 0, 3: 1, 8: 1, 9: 2} {
		n[i].meshIndex, n[i].skin = skinTestU32(0), skinTestU32(s)
	}
	ibm := func(k int) []float32 { // k joints, each a scaled, offset 3x4
		out := []float32{}
		for j := 0; j < k; j++ {
			f := float32(j + 1)
			out = append(out, 1/f, 0, 0, 0.1*f, 0, 1, 0, -0.2*f, 0, 0, 1, 0.3)
		}
		return out
	}
	skin := func(joints ...uint32) *Skin {
		return &Skin{joints: joints, inverseBindMatrices: ibm(len(joints)), texture: &GLTFTexture{tex: &skinTestTexture{}}}
	}
	return &Model{nodes: n,
		meshes: []*Mesh{{morphTargetWeights: GLTFAnimatableProperty{restValue: []float32{}}}},
		skins:  []*Skin{skin(1, 5, 6), skin(6), skin(5)},
		scenes: []*Scene{{nodes: []uint32{0, 4}}}}
}

// skinTestOldMatrices is calculateSkinMatrices as it was, returning its slice.
func skinTestOldMatrices(skin *Skin, inverseGlobalTransform mgl.Mat4, nodes []*Node) []float32 {
	matrices := make([]float32, len(skin.joints)*12*2)
	for i, joint := range skin.joints {
		n := nodes[joint]
		reverseBindMatrix := skin.inverseBindMatrices[i*12 : (i+1)*12]
		matrix := mgl.Ident4()
		for j, v := range reverseBindMatrix {
			matrix[j] = v
		}
		matrix = n.worldTransform.Mul4(matrix.Transpose())
		matrix = inverseGlobalTransform.Mul4(matrix).Transpose()
		for j := 0; j < 12; j++ {
			matrices[i*24+j] = matrix[j]
		}
		normalMatrix := matrix.Transpose().Inv().Transpose()
		for j := 0; j < 12; j++ {
			matrices[i*24+12+j] = normalMatrix[j]
		}
	}
	return matrices
}

// skinTestOldDraw is model.draw's first loop with the old skin step: every
// skinned mesh node computes and uploads. It returns what each skin's texture
// held at the end, and how many uploads were made.
func skinTestOldDraw(mdl *Model) ([][]float32, int) {
	held, uploads := make([][]float32, len(mdl.skins)), 0
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, index := range n.childrenIndex {
			walk(mdl.nodes[index])
		}
		if n.meshIndex != nil && n.skin != nil {
			held[*n.skin] = skinTestOldMatrices(mdl.skins[*n.skin], n.worldTransform.Inv(), mdl.nodes)
			uploads++
		}
	}
	scene := mdl.scenes[0]
	for _, index := range scene.nodes {
		mdl.nodes[index].calculateWorldTransform(mgl.Ident4(), mdl.nodes)
		walk(mdl.nodes[index])
	}
	return held, uploads
}

// skinTestNewDraw is model.draw's first loop as it is.
func skinTestNewDraw(mdl *Model) {
	scene := mdl.scenes[0]
	skinWriters := scene.skinWritersOf(mdl)
	for _, index := range scene.nodes {
		mdl.nodes[index].calculateWorldTransform(mgl.Ident4(), mdl.nodes)
		calculateAnimationData(mdl, mdl.nodes[index], skinWriters)
	}
}

func skinTestSameBits(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float32bits(a[i]) != math.Float32bits(b[i]) {
			return false
		}
	}
	return true
}

func TestSkinWritersAreTheLastSkinnedMeshPerSkin(t *testing.T) {
	mdl := skinTestModel()
	w := mdl.scenes[0].skinWritersOf(mdl)
	for skin, want := range []int{7, 8, 9} {
		if w[skin] != mdl.nodes[want] {
			t.Errorf("skin %d: writer is not node %d", skin, want)
		}
	}
}

// Draw by draw, each skin texture holds exactly what the old path left in it,
// and it is uploaded only on a draw where that content changed.
func TestSkinTexturesMatchTheOldPath(t *testing.T) {
	old, cur := skinTestModel(), skinTestModel()
	prev := make([][]float32, len(old.skins))
	wantUploads := make([]int, len(old.skins))
	oldUploads := 0
	moveJoint := func(m *Model) {
		m.nodes[6].rotation.animate([4]float32{0, 0.6, 0, 0.8})
		m.nodes[6].transformChanged = true
	}
	for draw := 1; draw <= 6; draw++ {
		if draw == 4 {
			moveJoint(old)
			moveJoint(cur)
		}
		held, n := skinTestOldDraw(old)
		oldUploads += n
		skinTestNewDraw(cur)
		for s := range cur.skins {
			if !skinTestSameBits(held[s], prev[s]) {
				wantUploads[s]++
			}
			prev[s] = held[s]
			tex := cur.skins[s].texture.tex.(*skinTestTexture)
			if !skinTestSameBits(tex.data, held[s]) {
				t.Fatalf("draw %d, skin %d: the texture holds other matrices than the old path's", draw, s)
			}
			if tex.uploads != wantUploads[s] {
				t.Fatalf("draw %d, skin %d: %d uploads, want %d", draw, s, tex.uploads, wantUploads[s])
			}
		}
	}
	// Six draws: the old path uploaded five times a draw (one per skinned mesh).
	// Now: once per skin, again after the joint moved (skins 0 and 1), and skin
	// 2 once more on draw 2, when its joint's world transform first exists.
	got := 0
	for _, s := range cur.skins {
		got += s.texture.tex.(*skinTestTexture).uploads
	}
	if oldUploads != 30 || got != 6 {
		t.Errorf("uploads: old %d (want 30), new %d (want 6)", oldUploads, got)
	}
}

// The morph-target lists are shared scratch now: a node's own list must not
// change when another node fills the scratch, and a texture upload still
// happens exactly when the set of active targets changes.
func TestMorphScratchKeepsEachNodesTargets(t *testing.T) {
	tex := func() *skinTestTexture { return &skinTestTexture{} }
	prim := func(t *skinTestTexture) *Primitive {
		mt := func(v float32) *MorphTarget { return &MorphTarget{positionBuffer: []float32{v, v, v, 0}} }
		return &Primitive{materialIndex: skinTestU32(0), morphTargets: []*MorphTarget{mt(1), mt(2), mt(3), mt(4)},
			morphTargetTexture: &GLTFTexture{tex: t}}
	}
	ta, tb := tex(), tex()
	a, b := skinTestNode([3]float32{}, [4]float32{0, 0, 0, 1}), skinTestNode([3]float32{}, [4]float32{0, 0, 0, 1})
	a.meshIndex, b.meshIndex = skinTestU32(0), skinTestU32(1)
	root := skinTestNode([3]float32{}, [4]float32{0, 0, 0, 1}, 1, 2)
	mdl := &Model{nodes: []*Node{root, a, b},
		meshes: []*Mesh{{morphTargetWeights: GLTFAnimatableProperty{restValue: []float32{}}, primitives: []*Primitive{prim(ta)}},
			{morphTargetWeights: GLTFAnimatableProperty{restValue: []float32{}}, primitives: []*Primitive{prim(tb)}}},
		scenes: []*Scene{{nodes: []uint32{0}}}}
	steps := []struct {
		wa, wb       []float32
		actA, actB   []uint32
		upA, upB     int
		weightA0     float32
		weightsCount uint32
	}{
		{[]float32{0, 0.5, 0, 1}, []float32{0.25, 0, 0, 0}, []uint32{1, 3}, []uint32{0}, 1, 1, 0.5, 2},
		{[]float32{0, 0.5, 0, 1}, []float32{0.25, 0, 0, 0}, []uint32{1, 3}, []uint32{0}, 1, 1, 0.5, 2},
		{[]float32{0.2, 0.5, 0, 0}, []float32{0, 0, 0.75, 0}, []uint32{0, 1}, []uint32{2}, 2, 2, 0.2, 2},
		{[]float32{0, 0, 0, 0}, []float32{0, 0, 0.75, 0}, []uint32{}, []uint32{2}, 2, 2, 0, 0},
		{[]float32{0, 0.5, 0, 1}, []float32{0, 0, 0.75, 0}, []uint32{1, 3}, []uint32{2}, 3, 2, 0.5, 2},
	}
	for i, s := range steps {
		a.morphTargetWeights.animate(s.wa)
		b.morphTargetWeights.animate(s.wb)
		skinTestNewDraw(mdl)
		if !skinTestSameU32(a.activeMorphTargets, s.actA) || !skinTestSameU32(b.activeMorphTargets, s.actB) {
			t.Fatalf("step %d: active targets a=%v b=%v, want %v %v", i, a.activeMorphTargets, b.activeMorphTargets, s.actA, s.actB)
		}
		if ta.uploads != s.upA || tb.uploads != s.upB {
			t.Fatalf("step %d: uploads a=%d b=%d, want %d %d", i, ta.uploads, tb.uploads, s.upA, s.upB)
		}
		p := mdl.meshes[0].primitives[0]
		if p.morphTargetWeight[0] != s.weightA0 || p.morphTargetCount != s.weightsCount {
			t.Fatalf("step %d: node a weight[0]=%v count=%d, want %v %d", i, p.morphTargetWeight[0], p.morphTargetCount, s.weightA0, s.weightsCount)
		}
	}
}

func skinTestSameU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
