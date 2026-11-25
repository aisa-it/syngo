package importmedia_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/importmedia"
	"github.com/aisa-it/syngo/internal/media"
)

// memRepo — метаданные в памяти (импорт пишет из нескольких потоков).
type memRepo struct {
	mu    sync.Mutex
	items map[string]media.Meta
}

func newMemRepo() *memRepo { return &memRepo{items: map[string]media.Meta{}} }

func (m *memRepo) UpsertImported(_ context.Context, meta media.Meta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items[meta.ContentURI] = meta

	return nil
}

func (m *memRepo) Imported(_ context.Context, contentURI string) (media.Meta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, ok := m.items[contentURI]
	if !ok {
		return media.Meta{}, media.ErrNotFound
	}

	return meta, nil
}

// dumpRecord разбирает строку дампа kind=media так же, как syngo-import-media.
func dumpRecord(t *testing.T, line string) importmedia.Record {
	t.Helper()

	var rec importmedia.Record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("строка дампа: %v", err)
	}

	return rec
}

// writeSynapseFile раскладывает файл по раскладке Synapse: local_content/ab/cd/rest.
func writeSynapseFile(t *testing.T, root, mediaID string, payload []byte) {
	t.Helper()

	key, err := importmedia.StorageKey(mediaID)
	if err != nil {
		t.Fatalf("storage key: %v", err)
	}

	path := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func newImporter(t *testing.T) (*importmedia.Importer, *memRepo, string) {
	t.Helper()

	source := t.TempDir()

	objects, err := media.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatalf("object store: %v", err)
	}

	repo := newMemRepo()

	return importmedia.New(repo, objects, source, "test.local"), repo, source
}

func TestImportCopiesAndHashes(t *testing.T) {
	t.Parallel()

	importer, repo, source := newImporter(t)

	payload := []byte("synapse media bytes")
	mediaID := "abcdef123456"

	writeSynapseFile(t, source, mediaID, payload)

	report, err := importer.Import(context.Background(), []importmedia.Record{{
		MediaID: mediaID, MediaType: "image/png", MediaLen: int64(len(payload)), UploadName: "pic.png",
	}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Copied != 1 || report.HasProblems() {
		t.Fatalf("отчёт: %+v", report)
	}

	meta, ok := repo.items["mxc://test.local/"+mediaID]
	if !ok {
		t.Fatal("метаданные не сохранены")
	}

	want := sha256.Sum256(payload)
	if !bytes.Equal(meta.SHA256, want[:]) {
		t.Fatalf("sha256 не совпал")
	}

	if meta.StorageKey == "" || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("метаданные: %+v", meta)
	}

	// Повторный импорт идемпотентен.
	report2, err := importer.Import(context.Background(), []importmedia.Record{{MediaID: mediaID, MediaLen: int64(len(payload))}})
	if err != nil {
		t.Fatalf("повторный импорт: %v", err)
	}

	if len(repo.items) != 1 {
		t.Fatalf("после повторного импорта записей %d, ожидали 1", len(repo.items))
	}

	// Повторный прогон байты не копирует - они уже в хранилище.
	if report2.Copied != 0 || report2.Skipped != 1 || report2.HasProblems() {
		t.Fatalf("повторный прогон должен пропустить уже перенесенное медиа, отчёт: %+v", report2)
	}
}

func TestImportMissingFileKeepsMetadata(t *testing.T) {
	t.Parallel()

	importer, repo, _ := newImporter(t)

	report, err := importer.Import(context.Background(), []importmedia.Record{{
		MediaID: "deadbeef00", MediaLen: 10,
	}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Missing != 1 || !report.HasProblems() {
		t.Fatalf("ожидали missing=1, отчёт: %+v", report)
	}

	if _, ok := repo.items["mxc://test.local/deadbeef00"]; !ok {
		t.Fatal("метаданные отсутствующего файла должны сохраняться")
	}

	if !bytes.Contains([]byte(report.ReportFile()), []byte("missing")) {
		t.Fatal("отчёт должен содержать статус missing")
	}
}

// TestImportSizeMismatch - при расхождении размера в метаданные
// пишется размер реальных байтов (Content-Length скачивания берется из БД),
// а не media_length дампа, иначе скачивание обрывалось.
func TestImportSizeMismatch(t *testing.T) {
	t.Parallel()

	importer, repo, source := newImporter(t)

	mediaID := "1234567890"
	writeSynapseFile(t, source, mediaID, []byte("12345"))

	report, err := importer.Import(context.Background(), []importmedia.Record{{
		MediaID: mediaID, MediaLen: 99,
	}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Mismatched != 1 || !report.HasProblems() {
		t.Fatalf("ожидали mismatch=1, отчёт: %+v", report)
	}

	if got := repo.items["mxc://test.local/"+mediaID].SizeBytes; got != 5 {
		t.Fatalf("size_bytes = %d, ожидали размер файла 5 (не 99 из дампа)", got)
	}

	if !strings.Contains(report.ReportFile(), "problem: "+mediaID) {
		t.Fatalf("расхождение должно быть в отчете:\n%s", report.ReportFile())
	}
}

// TestImportURLCacheSkipped - превью ссылок (url_cache) не переносятся
// и не считаются потерей: раньше давали массовый missing и ненулевой код.
func TestImportURLCacheSkipped(t *testing.T) {
	t.Parallel()

	importer, repo, _ := newImporter(t)

	rec := dumpRecord(t, `{"kind":"media","media_id":"urlcache0001","media_type":"image/png",`+
		`"media_length":10,"url_cache":"http://example.org/page"}`)

	report, err := importer.Import(context.Background(), []importmedia.Record{rec})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.HasProblems() || report.Missing != 0 {
		t.Fatalf("url_cache не должен давать missing и ошибку, отчёт: %+v", report)
	}

	if len(repo.items) != 0 {
		t.Fatalf("метаданные превью не переносятся: %+v", repo.items)
	}

	if !strings.Contains(report.ReportFile(), "url_cache") {
		t.Fatalf("пропуск превью должен быть виден в отчете:\n%s", report.ReportFile())
	}
}

// TestImportHashChecked - sha256 файла сверяется с sha256 из БД
// Synapse; раньше заявленная сверка сравнивала только размеры.
func TestImportHashChecked(t *testing.T) {
	t.Parallel()

	importer, _, source := newImporter(t)

	good, bad := []byte("good bytes"), []byte("rotten bytes")
	writeSynapseFile(t, source, "hashgood01", good)
	writeSynapseFile(t, source, "hashbad001", bad)

	goodSum := sha256.Sum256(good)
	wrongSum := sha256.Sum256([]byte("what synapse saw"))

	report, err := importer.Import(context.Background(), []importmedia.Record{
		dumpRecord(t, `{"media_id":"hashgood01","media_length":10,"sha256":"`+hex.EncodeToString(goodSum[:])+`"}`),
		dumpRecord(t, `{"media_id":"hashbad001","media_length":12,"sha256":"`+hex.EncodeToString(wrongSum[:])+`"}`),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if !report.HasProblems() || report.Copied != 1 {
		t.Fatalf("несовпадение sha256 должно быть проблемой, отчёт: %+v", report)
	}

	text := report.ReportFile()
	if !strings.Contains(text, "problem: hashbad001") || strings.Contains(text, "problem: hashgood01") {
		t.Fatalf("в отчете должна быть только испорченная запись:\n%s", text)
	}
}

// TestImportResumesAfterInterruption - прогон после обрыва копирует
// только то, чего нет в хранилище (или что лежит не целиком).
func TestImportResumesAfterInterruption(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	objectDir := filepath.Join(t.TempDir(), "objects")

	objects, err := media.NewLocalStore(objectDir)
	if err != nil {
		t.Fatalf("object store: %v", err)
	}

	repo := newMemRepo()
	importer := importmedia.New(repo, objects, source, "test.local")

	records := make([]importmedia.Record, 0, 3)

	for _, id := range []string{"resume0001", "resume0002", "resume0003"} {
		payload := []byte("payload of " + id)
		writeSynapseFile(t, source, id, payload)
		records = append(records, importmedia.Record{MediaID: id, MediaLen: int64(len(payload))})
	}

	if _, err := importer.Import(context.Background(), records); err != nil {
		t.Fatalf("первый прогон: %v", err)
	}

	// Обрыв: один объект не дописан, другого нет совсем.
	key2, _ := importmedia.StorageKey("resume0002")
	if err := os.WriteFile(filepath.Join(objectDir, filepath.FromSlash(key2)), []byte("payl"), 0o600); err != nil {
		t.Fatalf("обрезать объект: %v", err)
	}

	key3, _ := importmedia.StorageKey("resume0003")
	if err := os.Remove(filepath.Join(objectDir, filepath.FromSlash(key3))); err != nil {
		t.Fatalf("удалить объект: %v", err)
	}

	report, err := importer.Import(context.Background(), records)
	if err != nil {
		t.Fatalf("повторный прогон: %v", err)
	}

	if report.Copied != 2 || report.Skipped != 1 || report.HasProblems() {
		t.Fatalf("после обрыва копируются только недописанные, отчёт: %+v", report)
	}
}

func TestImportTooShortMediaID(t *testing.T) {
	t.Parallel()

	importer, _, _ := newImporter(t)

	report, err := importer.Import(context.Background(), []importmedia.Record{{MediaID: "ab"}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.FailedCount != 1 {
		t.Fatalf("ожидали failed=1, отчёт: %+v", report)
	}
}

// TestStorageKeyRejectsTraversal — дамп может содержать что угодно: путь из media_id не строим.
func TestStorageKeyRejectsTraversal(t *testing.T) {
	t.Parallel()

	for _, mediaID := range []string{
		"../etc/passwd",
		"ab/cd/ef",
		"..aaaa",
		"abc de",
		"abcdef\x00",
	} {
		if _, err := importmedia.StorageKey(mediaID); err == nil {
			t.Fatalf("media_id %q должен быть отвергнут", mediaID)
		}
	}
}

// TestImportTraversalFailsWithoutSideEffects — запись с опасным media_id даёт failed, файл не копируется.
func TestImportTraversalFailsWithoutSideEffects(t *testing.T) {
	t.Parallel()

	importer, repo, source := newImporter(t)

	// Кладём «секрет» рядом с media_store, чтобы увидеть, что его не прочитали.
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	report, err := importer.Import(context.Background(), []importmedia.Record{{
		MediaID: "../secret.txt", MediaLen: 6,
	}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.FailedCount != 1 {
		t.Fatalf("ожидали failed=1, отчёт: %+v", report)
	}

	if len(repo.items) != 0 {
		t.Fatalf("метаданные опасного media_id сохранять нельзя: %+v", repo.items)
	}
}

func TestStorageKeyLayout(t *testing.T) {
	t.Parallel()

	key, err := importmedia.StorageKey("abcdef123456")
	if err != nil {
		t.Fatalf("storage key: %v", err)
	}

	want := "local_content/ab/cd/ef123456"
	if key != want {
		t.Fatalf("key = %q, ожидали %q", key, want)
	}

	if _, err := importmedia.StorageKey("abc"); err == nil {
		t.Fatal("короткий media_id должен давать ошибку")
	}
}

// чтение через media.Service: байты, скопированные импортёром, скачиваются.
func TestImportedBytesAreDownloadable(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	objectDir := filepath.Join(t.TempDir(), "objects")

	objects, err := media.NewLocalStore(objectDir)
	if err != nil {
		t.Fatalf("object store: %v", err)
	}

	repo := newMemRepo()
	importer := importmedia.New(repo, objects, source, "test.local")

	payload := []byte("downloadable")
	mediaID := "ffffaaaabbbb"
	writeSynapseFile(t, source, mediaID, payload)

	if _, err := importer.Import(context.Background(), []importmedia.Record{{MediaID: mediaID, MediaLen: int64(len(payload)), MediaType: "text/plain"}}); err != nil {
		t.Fatalf("import: %v", err)
	}

	svc := media.NewService(newRepoAdapter(repo), objects, "test.local", 1<<20)

	meta, body, err := svc.Download(context.Background(), "test.local", mediaID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("скачанные байты не совпали: %q", got)
	}

	if meta.ContentType != "text/plain" {
		t.Fatalf("content-type = %q", meta.ContentType)
	}
}

// newRepoAdapter превращает memRepo в media.Repo (ByID/Insert/TouchAccess).
type repoAdapter struct{ m *memRepo }

func newRepoAdapter(m *memRepo) *repoAdapter { return &repoAdapter{m: m} }

func (a *repoAdapter) Insert(ctx context.Context, meta media.Meta) error {
	return a.m.UpsertImported(ctx, meta)
}

func (a *repoAdapter) ByID(_ context.Context, serverName, mediaID string) (media.Meta, error) {
	for i := range a.m.items {
		meta := a.m.items[i]
		if meta.ServerName == serverName && meta.MediaID == mediaID {
			return meta, nil
		}
	}

	return media.Meta{}, media.ErrNotFound
}

func (a *repoAdapter) TouchAccess(context.Context, string) error { return nil }

func (a *repoAdapter) InsertPending(ctx context.Context, meta media.Meta) error {
	return a.Insert(ctx, meta)
}

func (a *repoAdapter) UpdateUploaded(ctx context.Context, meta media.Meta) error {
	return a.Insert(ctx, meta)
}

func (a *repoAdapter) PendingStats(context.Context, string, time.Time) (int, time.Time, error) {
	return 0, time.Time{}, nil
}

func (a *repoAdapter) DeleteExpiredPending(context.Context, string, time.Time) error { return nil }

var _ = errors.Is
