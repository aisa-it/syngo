package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // регистрация декодеров
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/url"
	"path"
	"strings"
)

// ErrUnsupportedImage — исходное медиа не картинка (или формат не поддержан).
var ErrUnsupportedImage = errors.New("media: unsupported image")

// ErrBadThumbnailParams — некорректные width/height/method (400 M_INVALID_PARAM).
var ErrBadThumbnailParams = errors.New("media: invalid thumbnail parameters")

// ThumbnailParams — параметры миниатюры из запроса.
type ThumbnailParams struct {
	Width  int
	Height int
	Method string // crop | scale
}

// Normalize проверяет и нормализует параметры (спека: width/height обязательны).
func (p *ThumbnailParams) Normalize() error {
	if p.Width <= 0 || p.Height <= 0 {
		return ErrBadThumbnailParams
	}

	const maxDim = 1000
	if p.Width > maxDim || p.Height > maxDim {
		return ErrBadThumbnailParams
	}

	if p.Method == "" {
		p.Method = "scale"
	}

	if p.Method != "scale" && p.Method != "crop" {
		return ErrBadThumbnailParams
	}

	// Размеры приводим к фиксированному набору: иначе произвольные width/height
	// создают по файлу кеша на каждую пару и превращают кеш в свалку.
	// Снапим по большей стороне, вторую масштабируем с сохранением аспекта:
	// независимый снап 33x100 → 96x320 менял запрошенный бокс.
	if p.Width >= p.Height {
		snapped := snapThumbnailSize(p.Width)
		p.Height = clampThumbnailSize(int(math.Round(float64(p.Height) * float64(snapped) / float64(p.Width))))
		p.Width = snapped
	} else {
		snapped := snapThumbnailSize(p.Height)
		p.Width = clampThumbnailSize(int(math.Round(float64(p.Width) * float64(snapped) / float64(p.Height))))
		p.Height = snapped
	}

	return nil
}

// allowedThumbnailSizes — фиксированный набор сторон миниатюр (как в Synapse:
// 32, 96, 320, 640, 800). Запрос округляется вверх до ближайшего размера.
var allowedThumbnailSizes = []int{32, 96, 320, 640, 800}

// clampThumbnailSize держит вторую сторону в допустимом диапазоне, не снапя её:
// аспект важнее попадания в «круглый» размер.
func clampThumbnailSize(size int) int {
	if size < 1 {
		return 1
	}

	if maxSize := allowedThumbnailSizes[len(allowedThumbnailSizes)-1]; size > maxSize {
		return maxSize
	}

	return size
}

// snapThumbnailSize округляет сторону вверх до ближайшего допустимого размера.
func snapThumbnailSize(size int) int {
	for _, allowed := range allowedThumbnailSizes {
		if size <= allowed {
			return allowed
		}
	}

	return allowedThumbnailSizes[len(allowedThumbnailSizes)-1]
}

// Пределы декодирования исходника: DecodeConfig читается до decode, поэтому
// бомба вида 60000x60000 отвергается без выделения памяти под пиксели.
const (
	maxThumbnailSourceSide   = 8192
	maxThumbnailSourcePixels = 25_000_000
	// maxConcurrentThumbnails — семафор на генерацию: параллельные запросы не
	// должны складываться в пиковую память процесса.
	maxConcurrentThumbnails = 4
)

// thumbnailSem — общий семафор генерации миниатюр.
var thumbnailSem = make(chan struct{}, maxConcurrentThumbnails)

// Thumb - готовая миниатюра и ее Content-Type.
type Thumb struct {
	Data        []byte
	ContentType string
}

// thumbnailFormat - формат миниатюры по типу исходника: PNG и GIF могут быть
// прозрачными (аватары), в JPEG прозрачные пиксели стали бы черными.
func thumbnailFormat(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/png", "image/gif", "image/apng":
		return "png"
	default:
		return "jpeg"
	}
}

// thumbnailCacheKey - ключ кеша миниатюры: сервер, медиа, итоговые габариты,
// метод и формат. Method и размеры обязательны в ключе, иначе crop и scale одного
// размера отдают друг другу файл.
func thumbnailCacheKey(serverName, mediaID string, params ThumbnailParams, format string) string {
	ext := "jpg"
	if format == "png" {
		ext = "png"
	}

	return path.Join("thumbnails", url.PathEscape(serverName), url.PathEscape(mediaID),
		fmt.Sprintf("%dx%d-%s.%s", params.Width, params.Height, params.Method, ext))
}

// Thumbnail отдает миниатюру (PNG для PNG/GIF, иначе JPEG) с кешем в объектном
// хранилище. Доступ и ожидание async-загрузки - как у DownloadWith: проверка
// идет до кеша, иначе legacy-путь получал бы миниатюру медиа после freeze.
func (s *Service) Thumbnail(ctx context.Context, serverName, mediaID string, params ThumbnailParams, opts ReadOptions) (Thumb, error) {
	if err := params.Normalize(); err != nil {
		return Thumb{}, err
	}

	meta, err := s.resolve(ctx, serverName, mediaID, opts)
	if err != nil {
		// Оригинал чужого медиа больше лимита: миниатюру делает его сервер.
		if serverName != s.server && s.remote != nil && errors.Is(err, ErrTooLarge) {
			return s.remoteThumbnail(ctx, serverName, mediaID, params)
		}

		return Thumb{}, err
	}

	if !strings.HasPrefix(meta.ContentType, "image/") {
		return Thumb{}, ErrUnsupportedImage
	}

	format := thumbnailFormat(meta.ContentType)
	thumb := Thumb{ContentType: "image/" + format}
	key := thumbnailCacheKey(serverName, mediaID, params, format)

	if cached, err := s.objects.Get(ctx, key); err == nil {
		defer func() { _ = cached.Close() }()

		data, readErr := io.ReadAll(io.LimitReader(cached, 10<<20))
		if readErr == nil && len(data) > 0 {
			thumb.Data = data

			return thumb, nil
		}
	}

	// Семафор: генерация дорогая, параллельные запросы не должны складываться.
	select {
	case thumbnailSem <- struct{}{}:
		defer func() { <-thumbnailSem }()
	case <-ctx.Done():
		return Thumb{}, ctx.Err()
	}

	body, err := s.objects.Get(ctx, meta.StorageKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Thumb{}, ErrNotFound
		}

		return Thumb{}, fmt.Errorf("media: get object: %w", err)
	}

	defer func() { _ = body.Close() }()

	// Читаем исходник целиком (лимит загрузки уже проверен), чтобы DecodeConfig и
	// Decode работали по одним и тем же байтам.
	raw, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return Thumb{}, ErrUnsupportedImage
	}

	// Габариты проверяем ДО декодирования: иначе малененький по весу PNG с
	// огромными сторонами выделяет память под полный растр.
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Thumb{}, ErrUnsupportedImage
	}

	// Слишком большой исходник - 413 M_TOO_LARGE по спеке.
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxThumbnailSourceSide || config.Height > maxThumbnailSourceSide ||
		config.Width*config.Height > maxThumbnailSourcePixels {
		return Thumb{}, ErrTooLarge
	}

	source, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Thumb{}, ErrUnsupportedImage
	}

	target := resize(source, params)

	var buf bytes.Buffer
	if format == "png" {
		err = png.Encode(&buf, target)
	} else {
		err = jpeg.Encode(&buf, target, &jpeg.Options{Quality: 85})
	}

	if err != nil {
		return Thumb{}, fmt.Errorf("media: encode thumbnail: %w", err)
	}

	thumb.Data = buf.Bytes()

	// Кеш не критичен: ошибка записи не должна ломать ответ.
	if _, err := s.objects.Put(ctx, key, bytes.NewReader(thumb.Data)); err != nil {
		_ = err
	}

	return thumb, nil
}

// resize масштабирует с сохранением пропорций (scale) или обрезает по центру (crop).
//
// Промежуточный буфер не больше целевого размера: старая реализация для crop
// строила «растянутый» растр (PNG 1x2000 при crop 1000x1000 давала ~15 ГиБ), из
// которого потом вырезала окно. Увеличение не делаем — источник
// меньше запрошенного отдаём как есть по пропорциям.
func resize(source image.Image, params ThumbnailParams) image.Image {
	bounds := source.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()

	if sw == 0 || sh == 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}

	targetW, targetH := params.Width, params.Height

	if params.Method == "crop" {
		// Вписываем по большей стороне, но не растягиваем больше исходника.
		scale := math.Max(float64(targetW)/float64(sw), float64(targetH)/float64(sh))
		if scale > 1 {
			scale = 1
		}

		targetW = minInt(targetW, atLeast1(int(math.Round(float64(sw)*scale))))
		targetH = minInt(targetH, atLeast1(int(math.Round(float64(sh)*scale))))

		return sampleBox(source, bounds, targetW, targetH, true)
	}

	scale := math.Min(float64(targetW)/float64(sw), float64(targetH)/float64(sh))
	if scale > 1 {
		scale = 1
	}

	dstW := atLeast1(int(math.Round(float64(sw) * scale)))
	dstH := atLeast1(int(math.Round(float64(sh) * scale)))

	return sampleBox(source, bounds, dstW, dstH, false)
}

// sampleBox отдаёт растр dstW×dstH: nearest-neighbour по всему источнику
// (crop=false) или по центральному окну с аспектом цели (crop=true).
func sampleBox(source image.Image, bounds image.Rectangle, dstW, dstH int, crop bool) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	sw, sh := bounds.Dx(), bounds.Dy()
	srcX, srcY, srcW, srcH := bounds.Min.X, bounds.Min.Y, sw, sh

	if crop {
		// Центральное окно с аспектом цели.
		wantAspect := float64(dstW) / float64(dstH)
		srcAspect := float64(sw) / float64(sh)

		if srcAspect > wantAspect {
			srcW = atLeast1(int(math.Round(float64(sh) * wantAspect)))
			srcX = bounds.Min.X + (sw-srcW)/2
		} else if srcAspect < wantAspect {
			srcH = atLeast1(int(math.Round(float64(sw) / wantAspect)))
			srcY = bounds.Min.Y + (sh-srcH)/2
		}
	}

	// При уменьшении берём среднее по площадке пикселя источника (box-фильтр):
	// nearest-neighbour даёт зернистые миниатюры, а их показывает Element в списке
	// комнат и вложений. При увеличении/равном масштабе
	// среднее по одному пикселю эквивалентно выбору ближайшего.
	scaleX := float64(srcW) / float64(dstW)
	scaleY := float64(srcH) / float64(dstH)

	for y := 0; y < dstH; y++ {
		y0 := srcY + int(math.Floor(float64(y)*scaleY))
		y1 := srcY + int(math.Ceil(float64(y+1)*scaleY))
		if y1 <= y0 {
			y1 = y0 + 1
		}

		if y1 > srcY+srcH {
			y1 = srcY + srcH
		}

		for x := 0; x < dstW; x++ {
			x0 := srcX + int(math.Floor(float64(x)*scaleX))
			x1 := srcX + int(math.Ceil(float64(x+1)*scaleX))
			if x1 <= x0 {
				x1 = x0 + 1
			}

			if x1 > srcX+srcW {
				x1 = srcX + srcW
			}

			var r, g, b, a uint64

			var n uint64

			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := source.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}

			if n == 0 {
				dst.Set(x, y, source.At(x0, y0))

				continue
			}

			dst.Set(x, y, color.RGBA64{
				R: clampChannel(r / n), G: clampChannel(g / n),
				B: clampChannel(b / n), A: clampChannel(a / n),
			})
		}
	}

	return dst
}

// clampChannel приводит усреднённое значение к uint16 без переполнения:
// сумма по окну может превысить 65535 (gosec G115).
func clampChannel(v uint64) uint16 {
	if v > 0xffff {
		return 0xffff
	}

	return uint16(v)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}

// atLeast1 не даёт размеру выродиться в ноль при округлении.
func atLeast1(v int) int {
	if v < 1 {
		return 1
	}

	return v
}
