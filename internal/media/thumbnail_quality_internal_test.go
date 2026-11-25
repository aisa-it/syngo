package media

import (
	"image"
	"image/color"
	"testing"
)

// TestSampleBoxAverages — при уменьшении миниатюра должна усреднять пиксели
// источника (box-фильтр), а не брать ближайший: иначе шахматка из чёрного и
// белого превращается в чёрный (или белый) шум вместо серого.
func TestSampleBoxAverages(t *testing.T) {
	t.Parallel()

	const size = 64

	src := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if (x+y)%2 == 0 {
				src.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			} else {
				src.Set(x, y, color.RGBA{A: 255})
			}
		}
	}

	dst := sampleBox(src, src.Bounds(), 8, 8, false)

	r, g, b, _ := dst.At(4, 4).RGBA()
	// Среднее по площадке 8×8 шахматки — ровно половина.
	if r < 0x7000 || r > 0x9000 || g < 0x7000 || g > 0x9000 || b < 0x7000 || b > 0x9000 {
		t.Fatalf("ожидали ~50%% серого, получили r=%d g=%d b=%d", r, g, b)
	}
}
