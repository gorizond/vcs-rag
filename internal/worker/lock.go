package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireLock берёт эксклюзивный неблокирующий flock на <dir>/.worker.lock —
// защита от нескольких воркеров на одном MirrorsDir (FR-014). Возвращает
// release; при занятом локе — ошибка «воркер уже запущен».
func AcquireLock(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".worker.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("воркер уже запущен (lock: %s)", path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
