package backup

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
)

// dbPath is the database member. A snapshot larger than one capsule member travels as ordered
// dbPartPrefix+"000", "001", ... members instead, each at most capsule.MaxFileBytes, and a
// restore joins them back; capsules with the whole file stay valid (owner decision, KyNotes Q8 A).
const (
	dbPath       = "data/ky_server.db"
	dbPartPrefix = dbPath + ".part-"
)

// partBytes is the most one database part holds; tests lower it.
var partBytes = capsule.MaxFileBytes

// databaseMembers is the snapshot as capsule members: one file when it fits, ordered parts when
// it does not.
func databaseMembers(db []byte) []recoveryclient.File {
	if int64(len(db)) <= partBytes {
		return []recoveryclient.File{{Path: dbPath, Data: db, Mode: 0600}}
	}
	var parts []recoveryclient.File
	for i := 0; len(db) > 0; i++ {
		n := int(min(int64(len(db)), partBytes))
		parts = append(parts, recoveryclient.File{Path: fmt.Sprintf("%s%03d", dbPartPrefix, i), Data: db[:n], Mode: 0600})
		db = db[n:]
	}
	return parts
}

// warnNearCapsuleLimit logs once a payload reaches 80% of what one capsule can expand to, so an
// operator hears before backups start failing.
func warnNearCapsuleLimit(files []recoveryclient.File) {
	var total int64
	for _, f := range files {
		total += int64(len(f.Data))
	}
	if total*5 >= capsule.MaxExpandedBytes*4 {
		log.Printf("backup: WARNING the backup payload is %d MiB, over 80%% of the %d MiB one capsule holds; prune retained data or backups will fail", total>>20, capsule.MaxExpandedBytes>>20)
	}
}

// JoinDatabase writes dir/data/ky_server.db from the parts an extracted capsule carried, in
// order, and reports whether there were any. Parts must be numbered from 000 without a gap, and
// never sit beside a whole database. remove deletes the parts once the database is written.
func JoinDatabase(dir string, remove bool) (bool, error) {
	parts, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(dbPartPrefix)) + "*")
	if err != nil || len(parts) == 0 {
		return false, err
	}
	slices.Sort(parts)
	for i, p := range parts {
		if strings.TrimPrefix(filepath.ToSlash(p), filepath.ToSlash(dir)+"/") != fmt.Sprintf("%s%03d", dbPartPrefix, i) {
			return false, errors.New("backup: database parts are not numbered 000 onwards without a gap")
		}
	}
	target := filepath.Join(dir, filepath.FromSlash(dbPath))
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("backup: the capsule carries both a database and database parts")
	}
	tmp, err := os.OpenFile(target+".joining", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	for _, p := range parts {
		data, err := os.ReadFile(p)
		if err == nil {
			_, err = tmp.Write(data)
		}
		if err != nil {
			tmp.Close()
			return false, err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return false, err
	}
	if remove {
		for _, p := range parts {
			if err := os.Remove(p); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}
