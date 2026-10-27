package shutterfly

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"
)

// Identical reports whether two files' contents are the same: byte equality
// (size, then SHA256) or — for .jpg/.jpeg/.png — the same bounds and every
// pixel's RGB equal (alpha ignored). Decode failures fall back to bytes only.
// Ported from the Python files_are_different, minus its EXIF-orientation
// block (a no-op bug there).
func Identical(pathA, pathB string) bool {
	a, err := os.ReadFile(pathA)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(pathB)
	if err != nil {
		return false
	}
	return identicalData(pathA, a, b)
}

// identicalData compares raw bytes; name supplies the extension hint.
func identicalData(name string, a, b []byte) bool {
	if len(a) == len(b) && sha256.Sum256(a) == sha256.Sum256(b) {
		return true
	}
	if !isImageName(name) {
		return false
	}
	ia, _, err := image.Decode(bytes.NewReader(a))
	if err != nil {
		return false
	}
	ib, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return false
	}
	ba, bb := ia.Bounds(), ib.Bounds()
	if ba.Dx() != bb.Dx() || ba.Dy() != bb.Dy() {
		return false
	}
	conv := color.NRGBAModel
	for y := 0; y < ba.Dy(); y++ {
		for x := 0; x < ba.Dx(); x++ {
			ca := conv.Convert(ia.At(ba.Min.X+x, ba.Min.Y+y)).(color.NRGBA)
			cb := conv.Convert(ib.At(bb.Min.X+x, bb.Min.Y+y)).(color.NRGBA)
			if ca.R != cb.R || ca.G != cb.G || ca.B != cb.B {
				return false
			}
		}
	}
	return true
}

func isImageName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".jpg") || strings.HasSuffix(l, ".jpeg") || strings.HasSuffix(l, ".png")
}
