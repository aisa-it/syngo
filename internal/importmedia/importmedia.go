// Package importmedia переносит медиа из Synapse в наш медиа-репозиторий.
//
// Что делает:
//   - читает файлы из каталога Synapse (медиа-хранилище: local_content/<2>/<2>/<rest>);
//   - копирует байты в наш media.ObjectStore под тем же storage_key, что записан в метаданных;
//   - считает sha256 и размер, сверяет размер и sha256 с дампом (local_media_repository.sha256
//     Synapse), обновляет метаданные (sha256, реальный размер, storage_key, карантин);
//   - отсутствующие/битые файлы не роняют импорт: попадают в отчёт, метаданные остаются
//     (download такого медиа вернет 404);
//   - превью ссылок (строки с url_cache) не переносит: это кеш, syngod запросит превью
//     заново; в отчете у них свой статус, а не missing;
//   - карантин (quarantined_by) переносит: такое медиа не отдается;
//   - миниатюры не переносит: syngod строит их по запросу и кеширует.
//
// Импорт идемпотентен: повторный прогон не дублирует записи и не копирует байты, уже
// лежащие в хранилище с тем же размером и sha256 (статус skipped) - так же продолжается
// прогон после обрыва; метаданные (карантин) при этом обновляются.
package importmedia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/media"
)

// Record — метаданные одного медиа из дампа Synapse (kind=media).
type Record struct {
	MediaID    string `json:"media_id"`
	MediaType  string `json:"media_type"`
	MediaLen   int64  `json:"media_length"`
	UploadName string `json:"upload_name"`
	UserID     string `json:"user_id"`
	CreatedMS  int64  `json:"created_at_ms"`
	// QuarantinedBy - local_media_repository.quarantined_by: медиа в карантине.
	QuarantinedBy *string `json:"quarantined_by"`
	// URLCache - local_media_repository.url_cache: медиа скачано для превью ссылки.
	URLCache *string `json:"url_cache"`
	// SHA256 - local_media_repository.sha256 (hex), если Synapse его считал.
	SHA256 string `json:"sha256"`
}

// MetaRepo — обновление метаданных импортированного медиа.
type MetaRepo interface {
	// UpsertImported сохраняет метаданные импортированного медиа: вставка или обновление
	// sha256/size/storage_key/карантина для существующей записи (идемпотентно).
	UpsertImported(ctx context.Context, meta media.Meta) error
	// Imported возвращает уже сохраненные метаданные (включая заблокированные);
	// media.ErrNotFound - записи нет.
	Imported(ctx context.Context, contentURI string) (media.Meta, error)
}

// Статусы медиа в отчете.
const (
	StatusCopied   = "copied"
	StatusSkipped  = "skipped"   // байты уже в хранилище с тем же размером и sha256
	StatusURLCache = "url_cache" // превью ссылки: не переносится
	StatusMissing  = "missing"
	StatusMismatch = "mismatch" // размер файла не равен media_length
	// StatusHashMismatch - sha256 файла не равен sha256 из БД Synapse (файл испорчен).
	StatusHashMismatch = "hash_mismatch"
	StatusFailed       = "failed"
)

// Outcome — результат по одному медиа.
type Outcome struct {
	MediaID     string
	ContentURI  string
	Status      string
	Reason      string
	Bytes       int64
	SHA256      string
	Quarantined bool
}

// Report — итоги импорта медиа.
type Report struct {
	StartedAt    time.Time
	FinishedAt   time.Time
	Copied       int
	Skipped      int
	URLCache     int
	Missing      int
	Mismatched   int
	HashMismatch int
	FailedCount  int
	Quarantined  int
	BytesCopied  int64
	Outcomes     []Outcome
}

// HasProblems сообщает, были ли ошибки, требующие внимания оператора. Пропущенные
// превью ссылок и карантин - не ошибки: так задумано.
func (r Report) HasProblems() bool {
	return r.Missing > 0 || r.Mismatched > 0 || r.HashMismatch > 0 || r.FailedCount > 0
}

// Importer переносит медиа из каталога Synapse в наше хранилище.
type Importer struct {
	repo       MetaRepo
	objects    media.ObjectStore
	sourceDir  string // media_store Synapse: содержит local_content/...
	serverName string
	now        func() time.Time
	workers    int
	rehash     bool
}

// New создаёт импортёр. sourceDir — путь к каталогу медиа Synapse (media_store_path).
func New(repo MetaRepo, objects media.ObjectStore, sourceDir, serverName string) *Importer {
	return &Importer{
		repo:       repo,
		objects:    objects,
		sourceDir:  sourceDir,
		serverName: serverName,
		now:        time.Now,
		workers:    1,
	}
}

// SetWorkers задает число параллельных копирований (n < 1 - одно): копирование
// идет в окне простоя, последовательное было узким местом.
func (im *Importer) SetWorkers(n int) {
	im.workers = max(n, 1)
}

// SetRehash включает перечитывание объекта перед пропуском: повторный прогон
// пропускает медиа только если sha256 объекта в хранилище совпал с записанным
// (дольше, но ловит порчу хранилища). По умолчанию сверяются размер и sha256
// из БД с sha256 источника.
func (im *Importer) SetRehash(on bool) {
	im.rehash = on
}

// mediaIDPattern — допустимый media_id: base64url-подобный (буквы, цифры, «_», «-»),
// не короче 5 символов. Всё остальное — данные из внешнего дампа, которым нельзя доверять:
// «/» или «..» в media_id увели бы путь за пределы media_store (чтение произвольного файла).
var mediaIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{5,}$`)

// StorageKey возвращает раскладку Synapse для media_id (local_content/ab/cd/rest).
//
// Мы сохраняем её как есть: метаданные, импортированные ранее (PoC), ссылаются именно
// на этот ключ, поэтому файл и запись остаются согласованными без правки схемы.
//
// media_id валидируется по charset: дамп — внешние данные, путь из них не строим без проверки.
func StorageKey(mediaID string) (string, error) {
	if len(mediaID) < 5 {
		return "", fmt.Errorf("media_id %q is too short", mediaID)
	}

	if !mediaIDPattern.MatchString(mediaID) {
		return "", fmt.Errorf("media_id %q contains unsupported characters", mediaID)
	}

	return filepath.ToSlash(filepath.Join("local_content", mediaID[0:2], mediaID[2:4], mediaID[4:])), nil
}

// Import обрабатывает набор записей и возвращает отчёт.
//
// Ошибка на отдельной записи не прерывает импорт: она попадает в отчёт. Записи
// обрабатываются SetWorkers потоками, порядок строк отчета - порядок дампа.
func (im *Importer) Import(ctx context.Context, records []Record) (Report, error) {
	report := Report{StartedAt: im.now().UTC()}
	outcomes := make([]Outcome, len(records))
	done := make([]bool, len(records))

	jobs := make(chan int)

	var wg sync.WaitGroup

	for range min(im.workers, max(len(records), 1)) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range jobs {
				outcomes[i] = im.importOne(ctx, records[i])
				done[i] = true
			}
		}()
	}

	var ctxErr error

	for i := range records {
		if ctxErr = ctx.Err(); ctxErr != nil {
			break
		}

		jobs <- i
	}

	close(jobs)
	wg.Wait()

	for i := range outcomes {
		if !done[i] {
			continue
		}

		outcome := outcomes[i]
		report.Outcomes = append(report.Outcomes, outcome)

		if outcome.Quarantined {
			report.Quarantined++
		}

		switch outcome.Status {
		case StatusCopied:
			report.Copied++
			report.BytesCopied += outcome.Bytes
		case StatusSkipped:
			report.Skipped++
		case StatusURLCache:
			report.URLCache++
		case StatusMissing:
			report.Missing++
		case StatusMismatch:
			report.Mismatched++
			report.BytesCopied += outcome.Bytes
		case StatusHashMismatch:
			report.HashMismatch++
			report.BytesCopied += outcome.Bytes
		default:
			report.FailedCount++
		}
	}

	report.FinishedAt = im.now().UTC()

	return report, ctxErr
}

// importOne — импорт одного медиа.
func (im *Importer) importOne(ctx context.Context, rec Record) Outcome {
	out := Outcome{
		MediaID:     rec.MediaID,
		ContentURI:  "mxc://" + im.serverName + "/" + rec.MediaID,
		Quarantined: rec.QuarantinedBy != nil && *rec.QuarantinedBy != "",
	}

	// Превью ссылок - кеш Synapse (url_cache): раньше такие строки давали массовый
	// missing и ненулевой код, маскируя реальные потери.
	if rec.URLCache != nil && *rec.URLCache != "" {
		out.Status = StatusURLCache
		out.Reason = "превью ссылки не переносится: syngod запросит его заново"

		return out
	}

	storageKey, err := StorageKey(rec.MediaID)
	if err != nil {
		out.Status = StatusFailed
		out.Reason = err.Error()

		return out
	}

	sourcePath := filepath.Join(im.sourceDir, filepath.FromSlash(storageKey))

	info, err := os.Stat(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = StatusMissing
			out.Reason = "файл отсутствует в media_store: " + storageKey

			// Метаданные не теряем: download вернёт 404, но запись останется для отчёта.
			im.persist(ctx, rec, storageKey, nil, 0, &out)

			return out
		}

		out.Status = StatusFailed
		out.Reason = "stat: " + err.Error()

		return out
	}

	wantSum := strings.ToLower(rec.SHA256)

	if existing, ok := im.alreadyStored(ctx, out.ContentURI, storageKey, info.Size(), wantSum); ok {
		out.Status = StatusSkipped
		out.Bytes = existing.SizeBytes
		out.SHA256 = hex.EncodeToString(existing.SHA256)
		// Байты не копируем, метаданные (карантин, имя) - обновляем по источнику.
		im.persist(ctx, rec, storageKey, existing.SHA256, existing.SizeBytes, &out)

		return out
	}

	sum, written, err := im.copyToStore(ctx, storageKey, sourcePath)
	if err != nil {
		out.Status = StatusFailed
		out.Reason = err.Error()

		return out
	}

	out.Bytes = written
	out.SHA256 = hex.EncodeToString(sum)
	out.Status = StatusCopied

	switch {
	case wantSum != "" && out.SHA256 != wantSum:
		out.Status = StatusHashMismatch
		out.Reason = "sha256 файла " + out.SHA256 + " != в БД Synapse " + wantSum
	case rec.MediaLen > 0 && written != rec.MediaLen:
		out.Status = StatusMismatch
		out.Reason = fmt.Sprintf("размер файла %d != в дампе %d (в метаданные записан размер файла)", written, rec.MediaLen)
	}

	// Файл кладем и при расхождении: оператор решит по отчету, а метаданные
	// описывают реально лежащие байты (размер, sha256) - скачивание не обрывается.
	im.persist(ctx, rec, storageKey, sum, written, &out)

	return out
}

// alreadyStored - медиа уже перенесено прошлым прогоном: в БД sha256 и тот же
// storage_key, размер в БД и в хранилище равен размеру исходного файла, sha256
// совпадает с источником (если известен). С SetRehash объект еще и перечитывается.
func (im *Importer) alreadyStored(ctx context.Context, contentURI, storageKey string, size int64, wantSum string) (media.Meta, bool) {
	existing, err := im.repo.Imported(ctx, contentURI)
	if err != nil || len(existing.SHA256) != sha256.Size || existing.StorageKey != storageKey || existing.SizeBytes != size {
		return media.Meta{}, false
	}

	if wantSum != "" && hex.EncodeToString(existing.SHA256) != wantSum {
		return media.Meta{}, false
	}

	sum, stored, err := im.objectDigest(ctx, storageKey, im.rehash)
	if err != nil || stored != size {
		return media.Meta{}, false
	}

	if im.rehash && !bytes.Equal(sum, existing.SHA256) {
		return media.Meta{}, false
	}

	return existing, true
}

// objectStatter - необязательная возможность хранилища: размер объекта без чтения.
type objectStatter interface {
	Stat(ctx context.Context, key string) (int64, error)
}

// objectDigest возвращает размер объекта в хранилище и, если нужно, его sha256.
func (im *Importer) objectDigest(ctx context.Context, key string, withHash bool) (sum []byte, size int64, err error) {
	if statter, ok := im.objects.(objectStatter); ok && !withHash {
		size, err = statter.Stat(ctx, key)

		return nil, size, err
	}

	obj, err := im.objects.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}

	defer obj.Close()

	hasher := sha256.New()

	if size, err = io.Copy(hasher, obj); err != nil {
		return nil, 0, err
	}

	return hasher.Sum(nil), size, nil
}

// copyToStore копирует файл в объектное хранилище, считая sha256 и размер.
func (im *Importer) copyToStore(ctx context.Context, storageKey, sourcePath string) (sum []byte, written int64, err error) {
	f, err := os.Open(sourcePath) //nolint:gosec // путь собран из media_id и каталога оператора
	if err != nil {
		return nil, 0, fmt.Errorf("открыть файл: %w", err)
	}

	defer f.Close()

	hasher := sha256.New()

	written, err = im.objects.Put(ctx, storageKey, io.TeeReader(f, hasher))
	if err != nil {
		return nil, 0, fmt.Errorf("записать в хранилище: %w", err)
	}

	// Проверка после записи: то, что реально лежит в хранилище, должно давать тот же хеш.
	verify, verifiedBytes, err := im.objectDigest(ctx, storageKey, true)
	if err != nil {
		return nil, 0, fmt.Errorf("перечитать объект: %w", err)
	}

	if verifiedBytes != written {
		return nil, 0, fmt.Errorf("после записи размер %d != прочитано %d", written, verifiedBytes)
	}

	if !bytes.Equal(verify, hasher.Sum(nil)) {
		return nil, 0, errors.New("sha256 объекта в хранилище не совпал с прочитанным из media_store")
	}

	return verify, verifiedBytes, nil
}

// persist сохраняет метаданные: sha256/storage_key, размер байтов в хранилище
// (для отсутствующего файла - из дампа) и карантин.
func (im *Importer) persist(ctx context.Context, rec Record, storageKey string, sum []byte, size int64, out *Outcome) {
	meta := media.Meta{
		ContentURI:  "mxc://" + im.serverName + "/" + rec.MediaID,
		MediaID:     rec.MediaID,
		ServerName:  im.serverName,
		Uploader:    rec.UserID,
		ContentType: rec.MediaType,
		SizeBytes:   rec.MediaLen,
		SHA256:      sum,
		FileName:    rec.UploadName,
		StorageKey:  storageKey,
	}

	// Content-Length скачивания берется из БД: размер - реальных байтов.
	if size > 0 {
		meta.SizeBytes = size
	}

	if rec.QuarantinedBy != nil {
		meta.QuarantinedBy = *rec.QuarantinedBy
	}

	if rec.CreatedMS > 0 {
		meta.CreatedAt = time.UnixMilli(rec.CreatedMS).UTC()
	}

	if err := im.repo.UpsertImported(ctx, meta); err != nil {
		out.Status = StatusFailed

		reason := "сохранить метаданные: " + err.Error()
		if out.Reason != "" {
			reason = out.Reason + "; " + reason
		}

		out.Reason = reason
	}
}

// ReportFile отдаёт отчёт в текстовом виде (для -report и лога): итоги, затем
// расхождения (их надо разобрать), затем строка на каждое медиа.
func (r Report) ReportFile() string {
	var b strings.Builder

	fmt.Fprintf(&b, "import media report\n")
	fmt.Fprintf(&b, "started:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "finished: %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "duration: %s\n", r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond))
	fmt.Fprintf(&b, "copied=%d skipped=%d url_cache=%d missing=%d mismatch=%d hash_mismatch=%d failed=%d quarantined=%d bytes_copied=%d\n",
		r.Copied, r.Skipped, r.URLCache, r.Missing, r.Mismatched, r.HashMismatch, r.FailedCount, r.Quarantined, r.BytesCopied)

	for i := range r.Outcomes {
		o := r.Outcomes[i]
		switch o.Status {
		case StatusMissing, StatusMismatch, StatusHashMismatch, StatusFailed:
			fmt.Fprintf(&b, "problem: %s\t%s\t%s\n", o.MediaID, o.Status, o.Reason)
		}
	}

	for i := range r.Outcomes {
		o := r.Outcomes[i]
		fmt.Fprintf(&b, "%s\t%s\t%s\tbytes=%d\tsha256=%s\tquarantined=%t\t%s\n",
			o.MediaID, o.Status, o.ContentURI, o.Bytes, o.SHA256, o.Quarantined, o.Reason)
	}

	return b.String()
}
