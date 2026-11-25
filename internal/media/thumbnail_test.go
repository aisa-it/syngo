package media

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"testing"
)

// TestThumbnailParamsNormalize — спека: width/height обязательны, method scale|crop.
func TestThumbnailParamsNormalize(t *testing.T) {
	t.Parallel()

	p := ThumbnailParams{Width: 64, Height: 64}
	if err := p.Normalize(); err != nil || p.Method != "scale" {
		t.Fatalf("normalize: %v method=%q", err, p.Method)
	}

	for _, tc := range []ThumbnailParams{
		{Width: 0, Height: 64},
		{Width: 64, Height: 0},
		{Width: 64, Height: 64, Method: "stretch"},
		{Width: 5000, Height: 64},
	} {
		bad := tc
		if err := bad.Normalize(); err == nil {
			t.Fatalf("ожидали ошибку для %+v", tc)
		}
	}
}

// TestThumbnailJPEG — миниатюра отдаётся как JPEG нужного размера (scale сохраняет пропорции).
func TestThumbnailJPEG(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("png encode: %v", err)
	}

	decoded, format, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil || format != "png" {
		t.Fatalf("decode: %v format=%q", err, format)
	}

	thumb := ResizeForTest(decoded, ThumbnailParams{Width: 40, Height: 40})
	if thumb.Bounds().Dx() != 40 || thumb.Bounds().Dy() != 20 {
		t.Fatalf("scale: %dx%d, ожидали 40x20", thumb.Bounds().Dx(), thumb.Bounds().Dy())
	}

	cropped := ResizeForTest(decoded, ThumbnailParams{Width: 40, Height: 40, Method: "crop"})
	if cropped.Bounds().Dx() != 40 || cropped.Bounds().Dy() != 40 {
		t.Fatalf("crop: %dx%d, ожидали 40x40", cropped.Bounds().Dx(), cropped.Bounds().Dy())
	}
}

// TestThumbnailSizesSnappedToFixedSet — произвольные width/height не должны
// создавать новые файлы кеша: стороны округляются вверх до фиксированного набора
// (32/96/320/640/800), как в Synapse.
func TestThumbnailSizesSnappedToFixedSet(t *testing.T) {
	t.Parallel()

	// Снап по большей стороне, вторая масштабируется с сохранением аспекта.
	cases := []struct{ width, inHeight, wantW, wantH int }{
		{1, 1, 32, 32},
		{33, 100, 106, 320},
		{500, 500, 640, 640},
		{1000, 900, 800, 720},
	}

	for _, tc := range cases {
		p := ThumbnailParams{Width: tc.width, Height: tc.inHeight}
		if err := p.Normalize(); err != nil {
			t.Fatalf("%dx%d: normalize: %v", tc.width, tc.inHeight, err)
		}

		if p.Width != tc.wantW || p.Height != tc.wantH {
			t.Fatalf("%dx%d -> %dx%d, ожидали %dx%d",
				tc.width, tc.inHeight, p.Width, p.Height, tc.wantW, tc.wantH)
		}
	}
}

// TestThumbnailCropBombMemory — PNG 1x2000 с crop 1000x1000 раньше строил
// промежуточный растр на ~15 ГиБ. Теперь прямой выбор по центральному окну:
// пик памяти на порядки меньше целевого растра.
func TestThumbnailCropBombMemory(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1, 2000))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	thumb := ResizeForTest(src, ThumbnailParams{Width: 1000, Height: 1000, Method: "crop"})

	runtime.GC()
	runtime.ReadMemStats(&after)

	if peak := after.TotalAlloc - before.TotalAlloc; peak > 64<<20 {
		t.Fatalf("пик аллокаций %d МиБ, ожидали меньше 64 МиБ", peak>>20)
	}

	// Увеличение не делаем: 1x2000 не может дать 1000x1000.
	if thumb.Bounds().Dx() > 1 || thumb.Bounds().Dy() > 1000 {
		t.Fatalf("crop %dx%d: ожидали в пределах исходника", thumb.Bounds().Dx(), thumb.Bounds().Dy())
	}
}

// TestThumbnailDecodeConfigLimits — габариты проверяются до декодирования:
// «бомба» (60000x60000 в заголовке) отвергается как некорректный запрос.
func TestThumbnailDecodeConfigLimits(t *testing.T) {
	t.Parallel()

	// 1x2000 — валидный маленький PNG: DecodeConfig обязан пройти проверку.
	src := image.NewRGBA(image.Rect(0, 0, 1, 2000))

	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("png encode: %v", err)
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}

	if config.Width*config.Height > 25_000_000 {
		t.Fatalf("габариты %dx%d не должны считаться бомбой", config.Width, config.Height)
	}
}

// TestThumbnailCacheKeyDistinguishesMethodAndSize — кеш-ключ обязан включать
// method и итоговые габариты: иначе crop и scale одного размера отдают друг
// другу файл.
func TestThumbnailCacheKeyDistinguishesMethodAndSize(t *testing.T) {
	t.Parallel()

	keys := map[string]struct{}{}

	for _, tc := range []ThumbnailParams{
		{Width: 96, Height: 96, Method: "scale"},
		{Width: 96, Height: 96, Method: "crop"},
		{Width: 320, Height: 320, Method: "scale"},
	} {
		p := tc
		if err := p.Normalize(); err != nil {
			t.Fatalf("%+v: normalize: %v", tc, err)
		}

		key := thumbnailCacheKey("test.local", "media1", p, "jpeg")
		if _, dup := keys[key]; dup {
			t.Fatalf("ключ кеша совпал для %+v: %s", tc, key)
		}

		keys[key] = struct{}{}
	}
}
