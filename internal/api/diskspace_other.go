//go:build !(linux || darwin || freebsd)

package api

import "errors"

// diskSpace на прочих ОС не реализован: метрики места на томе медиа нет.
func diskSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.New("disk space: unsupported OS")
}
