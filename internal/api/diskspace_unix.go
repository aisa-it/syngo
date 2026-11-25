//go:build linux || darwin || freebsd

package api

import "syscall"

// diskSpace - свободное (доступное непривилегированному процессу) и полное
// место на файловой системе каталога.
func diskSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t

	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}

	bsize := uint64(st.Bsize) //nolint:gosec,unconvert // размер блока положителен; тип зависит от ОС

	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil //nolint:gosec,unconvert // тип зависит от ОС
}
