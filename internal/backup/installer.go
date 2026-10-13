package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/ky-primitives/keyfile"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const installerClientPath = "installer/client.secret"
const installerDirectoryPath = "installer/directory.secret"
const installerPathsPath = "installer/credential-paths.json"

// Read the snapshotted authority, not live state that may change after VACUUM INTO.
func installerSSO(ctx context.Context, db *sql.DB) (*applysetup.SSO, error) {
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key='installer.config'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bundle applysetup.Common
	if err = applysetup.DecodeRaw([]byte(raw), &bundle); err != nil {
		return nil, errors.New("backup: invalid installer configuration")
	}
	if err = store.ValidateInstallerBundle(bundle); err != nil {
		return nil, errors.New("backup: invalid installer configuration")
	}
	return bundle.SSO, nil
}

func installerFiles(sso *applysetup.SSO) ([]recoveryclient.File, error) {
	paths := map[string]string{installerClientPath: sso.ClientSecretFile, installerDirectoryPath: sso.DirectorySecretFile}
	var files []recoveryclient.File
	for _, name := range []string{installerClientPath, installerDirectoryPath} {
		secret, err := applysetup.Secret(paths[name])
		if err != nil {
			return nil, errors.New("backup: required installer credential unavailable")
		}
		files = append(files, recoveryclient.File{Path: name, Data: []byte(secret), Mode: 0600})
	}
	raw, err := json.Marshal(paths)
	if err != nil {
		return nil, err
	}
	return append(files, recoveryclient.File{Path: installerPathsPath, Data: raw, Mode: 0600}), nil
}

func installerCredentialCheck(dir string, required []string) recoveryclient.Check {
	check := recoveryclient.Check{Name: "Installer Credentials"}
	db, problem := openReadOnly(filepath.Join(dir, "data", "ky_server.db"))
	if db == nil {
		check.Message = problem
		return check
	}
	defer db.Close()
	sso, err := installerSSO(context.Background(), db)
	if err != nil {
		check.Message = "Cannot inspect retained installer configuration"
		return check
	}
	if sso == nil {
		check.Passed = true
		check.Message = "Unmanaged configuration"
		return check
	}
	for _, name := range []string{installerClientPath, installerDirectoryPath, installerPathsPath} {
		if !slices.Contains(required, name) {
			check.Message = "Required managed credential omitted"
			return check
		}
	}
	raw, err := keyfile.ReadPrivate(filepath.Join(dir, installerPathsPath), applysetup.MaxBundleBytes)
	if err != nil {
		check.Message = "Managed credential mapping missing or unsafe"
		return check
	}
	var paths map[string]string
	if applysetup.DecodeRaw(raw, &paths) != nil || len(paths) != 2 || paths[installerClientPath] != sso.ClientSecretFile || paths[installerDirectoryPath] != sso.DirectorySecretFile {
		check.Message = "Managed credential mapping conflicts with retained authority"
		return check
	}
	for _, name := range []string{installerClientPath, installerDirectoryPath} {
		if _, err := applysetup.Secret(filepath.Join(dir, name)); err != nil {
			check.Message = "Managed credential missing, malformed or unsafe"
			return check
		}
	}
	check.Passed = true
	check.Message = "Managed credentials and restore mapping verified"
	return check
}
