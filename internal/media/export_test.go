package media

import "image"

// ResizeForTest — тестовый доступ к resize (в проде вызывается только из Thumbnail).
// Живёт в _test.go, чтобы не расширять публичный API пакета.
func ResizeForTest(source image.Image, params ThumbnailParams) image.Image {
	return resize(source, params)
}
