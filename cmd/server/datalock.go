package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// lockDataDir holds an exclusive lock on <dir>/server.lock for the server's lifetime: a second
// server on the same data directory would reconcile the running one's in-flight commands as its
// own restart. The kernel drops the lock when the process ends. A filesystem without locks is
// logged and not refused.
func lockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		_ = f.Close()
		return nil, fmt.Errorf("another kyyard-server is already running on %s", dir)
	}
	log.Printf("[KYYARD] %s cannot be locked (%v): nothing stops a second server on this data directory", dir, err)
	return f, nil
}
