// Package build хранит сведения о сборке, подставляемые линкером.
//
// Значения задаются при сборке:
//
//	go build -ldflags "-X .../internal/build.Version=v1.165.0 -X .../internal/build.Commit=abc123"
//
// Схема версий: v1.<minor Synapse>.<патч syngo>. Minor совпадает с версией Synapse,
// с которой проверена совместимость (SynapseCompat): перенос БД (syngo-export,
// syngo-import), федерация (интероп-стенд) и поведение Client-Server API.
// Патч - релизы syngo на той же базе Synapse.
package build

import "strings"

// Name - имя реализации (поле server.name в /_matrix/federation/v1/version).
const Name = "syngo"

// RepositoryURL - общедоступное место получения Corresponding Source.
const RepositoryURL = "https://github.com/aisa-it/syngo"

// LicenseID - SPDX-идентификатор лицензии всего продукта.
const LicenseID = "AGPL-3.0-or-later"

// SynapseCompat - версия Synapse (major.minor), с которой проверена совместимость.
// Меняется вместе с образом Synapse интероп-стендов и фикстурами импорта - это
// проверяет TestSynapseCompatMatchesStands.
const SynapseCompat = "1.165"

// Version — версия сборки: тег git (`git describe --tags`, например "v1.165.0") или "dev".
var Version = "dev"

// Commit — git-хэш (короткий) на момент сборки.
var Commit = "unknown"

// BuildTime — время сборки в RFC 3339 (UTC).
var BuildTime = "unknown"

// Semver - версия без префикса "v" (для /version и метрик): "1.165.0".
func Semver() string {
	return strings.TrimPrefix(Version, "v")
}

// SourceURL - ссылка на исходники собранной ревизии. Релизная сборка обязана
// задавать Commit; без него возвращается общий адрес репозитория.
func SourceURL() string {
	commit := strings.TrimSpace(Commit)
	if commit == "" || strings.EqualFold(commit, "unknown") {
		return RepositoryURL
	}

	return RepositoryURL + "/tree/" + commit
}

// String - строка версии для `-version` и логов:
// "syngo v1.165.0 (Synapse 1.165 compatible, commit abc123, built 2026-11-23T10:00:00Z)".
func String() string {
	return Name + " " + Version + " (Synapse " + SynapseCompat + " compatible, commit " + Commit + ", built " + BuildTime + ")"
}
