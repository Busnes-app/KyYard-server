package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/config"
	"golang.org/x/sys/unix"
)

var ErrDrillBusy = errors.New("backup: a restore drill is already running")

// RunDrill serializes HTTP and CLI drills sharing a data directory. The persistent
// lock file must not be unlinked: all processes must lock the same inode. Closing the
// descriptor (including process exit) releases the advisory lock.
func RunDrill(ctx context.Context, cfg *config.Config, payload recoveryclient.Payload) (*recoveryclient.DrillResult, error) {
	lock, err := os.OpenFile(filepath.Join(cfg.Database.DataDir, "drill.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrDrillBusy
		}
		return nil, err
	}
	root := DrillRoot(cfg)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	return recoveryclient.Drill(ctx, root, payload, Checks)
}
