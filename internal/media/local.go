package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalStore — объектное хранилище на локальном каталоге (dev и небольшие инсталляции).
//
// Ключи генерируются сервисом (serverName/mediaID), но проверка пути всё равно есть:
// хранилище не должно зависеть от аккуратности вызывающего.
type LocalStore struct {
	dir string
}

// NewLocalStore создаёт каталог (если нужно) и проверяет, что он доступен на запись.
func NewLocalStore(dir string) (*LocalStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("media: local store directory is empty")
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("media: create local store dir: %w", err)
	}

	probe := filepath.Join(dir, ".write-probe")

	f, err := os.Create(probe)
	if err != nil {
		return nil, fmt.Errorf("media: local store is not writable: %w", err)
	}

	_ = f.Close()
	_ = os.Remove(probe)

	return &LocalStore{dir: dir}, nil
}

// Put сохраняет объект атомарно: пишем во временный файл и переименовываем.
func (s *LocalStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	path, err := s.safePath(key)
	if err != nil {
		return 0, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return 0, fmt.Errorf("media: mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("media: temp file: %w", err)
	}

	tmpName := tmp.Name()

	written, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()

	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)

		if copyErr != nil {
			return 0, fmt.Errorf("media: write object: %w", copyErr)
		}

		return 0, fmt.Errorf("media: close object: %w", closeErr)
	}

	select {
	case <-ctx.Done():
		_ = os.Remove(tmpName)

		return 0, ctx.Err()
	default:
	}

	if err := os.Chmod(tmpName, 0o640); err != nil {
		_ = os.Remove(tmpName)

		return 0, fmt.Errorf("media: chmod object: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return 0, fmt.Errorf("media: rename object: %w", err)
	}

	return written, nil
}

// Get открывает объект на чтение.
func (s *LocalStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.safePath(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("media: open object: %w", err)
	}

	return f, nil
}

// Delete удаляет объект; отсутствие файла не считается ошибкой.
func (s *LocalStore) Delete(_ context.Context, key string) error {
	path, err := s.safePath(key)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("media: delete object: %w", err)
	}

	return nil
}

// safePath превращает ключ в путь внутри каталога хранилища и запрещает выход наружу.
func (s *LocalStore) safePath(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("media: invalid storage key %q", key)
	}

	clean := filepath.Clean(key)
	if clean != key {
		return "", fmt.Errorf("media: invalid storage key %q", key)
	}

	return filepath.Join(s.dir, clean), nil
}

// Stat возвращает размер объекта без чтения (повторный импорт медиа).
func (s *LocalStore) Stat(_ context.Context, key string) (int64, error) {
	path, err := s.safePath(key)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}

		return 0, fmt.Errorf("media: stat object: %w", err)
	}

	return info.Size(), nil
}
