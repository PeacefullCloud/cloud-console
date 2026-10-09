package auth

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitialPasswordFile is where a generated bootstrap password is kept.
const InitialPasswordFile = "initial-admin-password"

// StoreInitialPassword writes a generated bootstrap password to a file only
// its owner can read, and returns the path. Log output is often shipped to
// other systems, so the password itself must never go there.
func StoreInitialPassword(dataDir, username, password string) (string, error) {
	path := filepath.Join(dataDir, InitialPasswordFile)

	// O_EXCL with 0600 from the start: the file is never readable by others,
	// not even briefly, and a stale one is replaced rather than appended to.
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := fmt.Fprintf(f, "username: %s\npassword: %s\n", username, password)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	return path, nil
}
