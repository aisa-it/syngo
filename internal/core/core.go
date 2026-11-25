// Package core содержит доменную модель homeserver'а: сущности, инварианты и
// сервисы, не зависящие от транспорта (HTTP) и хранилища (SQL).
//
// Пакет не импортирует internal/api и internal/store — зависимости идут
// в одну сторону: api → core ← store.
package core

import "fmt"

// SpecVersion — целевая версия Matrix Client-Server API, на которую опирается
// сервер. Версия замораживается на релизный цикл.
const SpecVersion = "v1.11"

// minorVersions — поддерживаемые минорные версии CS-API. Список растёт по мере
// прохождения Complement; порядок — возрастающий.
var minorVersions = []string{
	"v1.1", "v1.2", "v1.3", "v1.4", "v1.5", "v1.6",
	"v1.7", "v1.8", "v1.9", "v1.10", SpecVersion,
}

// SupportedVersions возвращает копию списка поддерживаемых версий CS-API в
// формате, ожидаемом GET /_matrix/client/versions.
func SupportedVersions() []string {
	out := make([]string, len(minorVersions))
	copy(out, minorVersions)

	return out
}

// UnstableFeatures перечисляет включённые экспериментальные возможности
// (MSC). org.matrix.simplified_msc3575 - simplified sliding sync (MSC4186): по
// нему Element X решает, что сервер его поддерживает.
func UnstableFeatures() map[string]bool {
	return map[string]bool{"org.matrix.simplified_msc3575": true}
}

// ErrNotFound — базовая ошибка «сущность не найдена», которую транспорт
// отображает в HTTP 404. Конкретные сущности оборачивают её через %w.
var ErrNotFound = fmt.Errorf("not found")
