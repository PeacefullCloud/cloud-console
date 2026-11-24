package backups

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/jobs"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/secrets"
)

func TestDestinationSecretsAndTenantScope(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sealer, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetSealer(sealer); err != nil {
		t.Fatal(err)
	}

	u1, err := db.CreateUser("alice", "pw", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := db.CreateUser("bob", "pw", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}

	id, err := db.CreateBackupDestination(&models.BackupDestination{
		UserID:        u1.ID,
		Name:          "alice-s3",
		Kind:          "s3",
		RepoURL:       "s3:https://s3.example.com/bucket",
		RepoPassword:  "super-secret-repo-pass",
		S3AccessKey:   "AKIA123",
		S3SecretKey:   "s3-secret-xyz",
		S3Region:      "eu-central-1",
		KeepLast:      5,
		KeepDaily:     7,
		KeepWeekly:    4,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.GetBackupDestination(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoPassword != "super-secret-repo-pass" || got.S3SecretKey != "s3-secret-xyz" {
		t.Fatalf("unsealed secrets mismatch: %+v", got)
	}

	aliceDests, err := db.ListBackupDestinationsScoped(models.Scope{UserID: u1.ID})
	if err != nil || len(aliceDests) != 1 {
		t.Fatalf("alice dests = %d, err = %v", len(aliceDests), err)
	}
	bobDests, err := db.ListBackupDestinationsScoped(models.Scope{UserID: u2.ID})
	if err != nil || len(bobDests) != 0 {
		t.Fatalf("bob dests = %d, err = %v", len(bobDests), err)
	}
}

func TestParseSFTPRepoAndKeyOpts(t *testing.T) {
	userHost, port, err := parseSFTPRepo("sftp:u12345@u12345.your-storagebox.de:23/backups")
	if err != nil {
		t.Fatal(err)
	}
	if userHost != "u12345@u12345.your-storagebox.de" || port != "23" {
		t.Fatalf("got %q port %q", userHost, port)
	}

	if _, _, err := parseSFTPRepo("sftp:bad;user@host/path"); err == nil {
		t.Fatal("expected shell metacharacter rejection")
	}
	if _, _, err := parseSFTPRepo("sftp:-oProxyCommand=id@host/path"); err == nil {
		t.Fatal("expected leading hyphen SSH option injection in user to be rejected")
	}
	if _, _, err := parseSFTPRepo("sftp:user@-oProxyCommand=id/path"); err == nil {
		t.Fatal("expected leading hyphen SSH option injection in host to be rejected")
	}

	if err := ValidateDestination(&models.BackupDestination{
		Name: "local-nonadmin", Kind: "local", RepoURL: "/var/tmp/backups",
	}, false); err == nil {
		t.Fatal("expected non-admin local destination to be rejected")
	}
	if err := ValidateDestination(&models.BackupDestination{
		Name: "local-etc", Kind: "local", RepoURL: "/etc/caddy",
	}, true); err == nil {
		t.Fatal("expected restricted /etc path to be rejected")
	}
	if err := ValidateDestination(&models.BackupDestination{
		Name: "s3-loopback", Kind: "s3", RepoURL: "s3:http://127.0.0.1:2019/bucket",
	}, true); err == nil {
		t.Fatal("expected loopback SSRF S3 endpoint to be rejected")
	}
	if err := ValidateDestination(&models.BackupDestination{
		Name: "s3-imds", Kind: "s3", RepoURL: "s3:http://169.254.169.254/latest",
	}, true); err == nil {
		t.Fatal("expected link-local metadata SSRF S3 endpoint to be rejected")
	}

	dest := &models.BackupDestination{
		Kind:          "sftp",
		RepoURL:       "sftp:u12345@box.example.com:23/backups",
		SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\ntest\n-----END OPENSSH PRIVATE KEY-----\n",
		SSHKnownHosts: "box.example.com ssh-ed25519 AAAA",
	}
	opts, cleanup, err := prepareSFTPOpts(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 2 || opts[0] != "-o" || !strings.Contains(opts[1], "sftp.command=ssh -i ") || !strings.Contains(opts[1], "-p 23 -- u12345@box.example.com -s sftp") {
		t.Fatalf("unexpected sftp opts: %#v", opts)
	}
	cleanup()
}

func TestResticAutoInitPruneAndCheck(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "restic.log")
	stateFile := filepath.Join(dir, "inited")
	fakeRestic := filepath.Join(dir, "restic")

	script := `#!/bin/sh
echo "$@" >> ` + logFile + `
for arg in "$@"; do
  case "$arg" in
    cat)
      if [ ! -f ` + stateFile + ` ]; then
        echo "Fatal: unable to open config file" >&2
        exit 10
      fi
      exit 0
      ;;
    init)
      touch ` + stateFile + `
      echo '{"message_type":"initialized","id":"abc"}'
      exit 0
      ;;
    forget)
      echo '[{"keep":[{"short_id":"1111aaaa"}],"remove":[{"short_id":"2222bbbb"}]}]'
      exit 0
      ;;
    snapshots)
      echo '[{"id":"1111aaaa00000000","short_id":"1111aaaa"}]'
      exit 0
      ;;
    check)
      echo '{"message_type":"summary"}'
      exit 0
      ;;
  esac
done
exit 0
`
	if err := os.WriteFile(fakeRestic, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	db, err := database.Open(filepath.Join(dir, "console.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &config.Config{DataDir: dir}
	jm := jobs.New(db, log, 1)
	app := &core.App{Cfg: cfg, DB: db, Jobs: jm, Log: log}
	svc := New(app)
	svc.SetResticBinary(fakeRestic)

	u, err := db.CreateUser("admin", "pw", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	destID, err := db.CreateBackupDestination(&models.BackupDestination{
		UserID:       u.ID,
		Name:         "local-repo",
		Kind:         "local",
		RepoURL:      filepath.Join(dir, "repo"),
		RepoPassword: "pw",
		KeepLast:     1,
		KeepDaily:    0,
		KeepWeekly:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := db.GetBackupDestination(destID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ensureResticRepoInit(context.Background(), dest, "vm1"); err != nil {
		t.Fatalf("ensureResticRepoInit: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("expected restic init to run: %v", err)
	}

	if _, err := db.SaveResticSnapshot(&models.ResticSnapshot{
		DestinationID: dest.ID, InstanceName: "vm1", SnapshotID: "1111aaaa", RepoPrefix: RepoURLForInstance(dest, "vm1"), Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveResticSnapshot(&models.ResticSnapshot{
		DestinationID: dest.ID, InstanceName: "vm1", SnapshotID: "2222bbbb", RepoPrefix: RepoURLForInstance(dest, "vm1"), Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}

	prunePayload, _ := json.Marshal(resticMaintenancePayload{DestinationID: dest.ID, Instance: "vm1"})
	if err := svc.handleResticPrune(context.Background(), &models.Job{ID: "j1", Payload: string(prunePayload)}); err != nil {
		t.Fatalf("handleResticPrune: %v", err)
	}
	snaps, err := db.ListResticSnapshotsForInstance("vm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].SnapshotID != "1111aaaa" {
		t.Fatalf("expected only 1111aaaa after prune sync, got %+v", snaps)
	}

	checkPayload, _ := json.Marshal(resticMaintenancePayload{DestinationID: dest.ID})
	if err := svc.handleResticCheck(context.Background(), &models.Job{ID: "j2", Payload: string(checkPayload)}); err != nil {
		t.Fatalf("handleResticCheck: %v", err)
	}
	updated, err := db.GetBackupDestination(dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastCheckStatus != "ok" {
		t.Fatalf("LastCheckStatus = %q, want ok", updated.LastCheckStatus)
	}
}

func TestMapResticExitCodes(t *testing.T) {
	runExit := func(code int) error {
		cmd := exec.Command("sh", "-c", "exit "+string(rune('0'+code)))
		if code >= 10 {
			cmd = exec.Command("sh", "-c", "exit 1"+string(rune('0'+(code-10))))
		}
		return cmd.Run()
	}
	for _, tc := range []struct {
		code int
		want string
	}{
		{10, "not initialized"},
		{11, "locked"},
		{12, "incorrect restic repository password"},
		{3, "partial read error"},
	} {
		err := mapResticError(runExit(tc.code), "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("code %d -> %v, want %q", tc.code, err, tc.want)
		}
	}
	if !errors.Is(mapResticError(nil, ""), nil) {
		t.Fatal("nil error should return nil")
	}
}
