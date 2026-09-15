package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Busness-app/ky-primitives/keyfile"
	"golang.org/x/sys/unix"
)

// The container's mounted data directory may already exist with mode 0755.
// Secure the opened directory itself; never chmod through a final symlink.
func secureDataDir(path string) error {
	path = filepath.Clean(path)
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open private directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	defer dir.Close()
	return dir.Chmod(0700)
}

func loadKey(dir, name, env string) ([]byte, error) {
	if env != "" {
		key, set, err := keyfile.FromEnv(env, 32)
		if err != nil {
			return nil, err
		}
		if set {
			return key, nil
		}
	}
	path := filepath.Join(dir, name)
	// Refuse special files before the helper opens them (a FIFO could block startup).
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: key must be a regular file", name)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	key, err := keyfile.LoadOrCreate(path, 32)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return key, nil
}
