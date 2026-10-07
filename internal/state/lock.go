package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes an exclusive, non-blocking lock in the state directory, so a
// cron run that is still sleeping out its random delay and a run started by
// hand (or by `lhc serve`) cannot both diff and overwrite the same baselines.
// The kernel drops the lock if the process dies; unlock releases it early.
func Lock(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
