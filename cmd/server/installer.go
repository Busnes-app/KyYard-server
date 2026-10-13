package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func installerSocket(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, ".installer", "setup.sock"), nil
}

func runInstaller(args []string) error {
	opts, err := applysetup.Parse(args)
	if err != nil {
		return err
	}
	if opts.Capabilities {
		return applysetup.WriteCapabilities(os.Stdout, "kyyard", "kyidentity-oidc", "directory-webhook", "dedicated-admin", "check")
	}
	if opts.Restart {
		return errors.New("KyYard installer configuration activates without restart")
	}
	var b applysetup.Common
	if err = applysetup.Decode(opts.File, &b); err != nil {
		return err
	}
	if err = store.ValidateInstallerBundle(b); err != nil {
		return err
	}
	dir := os.Getenv("KY_DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	socket, err := installerSocket(dir)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	r, err := applysetup.CallLocal(context.Background(), socket, raw, opts.Check)
	if err != nil {
		return err
	}
	if r.Service != b.Service || r.SetupID != b.SetupID {
		return errors.New("product setup report identity mismatch")
	}
	return r.Write(os.Stdout)
}
