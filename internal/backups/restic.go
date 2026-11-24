package backups

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/models"
)

// Restic exit codes that map to actionable UI errors.
const (
	resticExitPartial = 3
	resticExitNoRepo  = 10
	resticExitLocked  = 11
	resticExitBadPass = 12
)

type resticBackupPayload struct {
	DestinationID int64  `json:"destination_id"`
	Instance      string `json:"instance"`
	BackupName    string `json:"backup_name"`
}

type resticRestorePayload struct {
	SnapshotRowID  int64  `json:"snapshot_row_id"`
	DestinationID  int64  `json:"destination_id"`
	SourceInstance string `json:"source_instance"`
	TargetInstance string `json:"target_instance"`
	SnapshotID     string `json:"snapshot_id"`
}

type resticMaintenancePayload struct {
	DestinationID int64  `json:"destination_id"`
	Instance      string `json:"instance,omitempty"`
}

// RepoURLForInstance returns the per-instance repository prefix under a
// destination so prune locks on one VM never block backups on another.
func RepoURLForInstance(dest *models.BackupDestination, instanceName string) string {
	base := strings.TrimRight(strings.TrimSpace(dest.RepoURL), "/")
	return base + "/" + instanceName
}

func (s *Service) resticExecutable() string {
	if s.resticBin != "" {
		return s.resticBin
	}
	return "restic"
}

func (s *Service) resticCacheDir(destID int64, instanceName string) string {
	base := s.app.Cfg.DataDir
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "restic-cache", fmt.Sprintf("dest-%d-%s", destID, instanceName))
}

func (s *Service) buildResticEnv(dest *models.BackupDestination, instanceName string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"RESTIC_REPOSITORY=" + RepoURLForInstance(dest, instanceName),
		"RESTIC_PASSWORD=" + dest.RepoPassword,
	}
	if dest.Kind == "s3" {
		if dest.S3AccessKey != "" {
			env = append(env, "AWS_ACCESS_KEY_ID="+dest.S3AccessKey)
		}
		if dest.S3SecretKey != "" {
			env = append(env, "AWS_SECRET_ACCESS_KEY="+dest.S3SecretKey)
		}
		if dest.S3Region != "" {
			env = append(env, "AWS_DEFAULT_REGION="+dest.S3Region)
		}
	}
	return env
}

// ValidateDestination checks a Restic backup destination for injection, path
// traversal, and SSRF before it is saved or executed.
func ValidateDestination(dest *models.BackupDestination, isAdmin bool) error {
	if dest == nil {
		return errors.New("destination is required")
	}
	if strings.TrimSpace(dest.Name) == "" {
		return errors.New("destination name is required")
	}
	if strings.ContainsAny(dest.RepoURL, "\x00\r\n") {
		return errors.New("invalid characters in repository URL")
	}
	switch dest.Kind {
	case "sftp":
		if _, _, err := parseSFTPRepo(dest.RepoURL); err != nil {
			return err
		}
		if strings.ContainsAny(dest.SSHKnownHosts, "\x00\r") {
			return errors.New("invalid characters in SSH known_hosts")
		}
		return nil
	case "s3":
		return validateS3Repo(dest.RepoURL, isAdmin)
	case "local":
		if !isAdmin {
			return errors.New("only administrators may configure local host-filesystem backup destinations")
		}
		return validateLocalRepo(dest.RepoURL)
	default:
		return errors.New("destination kind must be s3, sftp or local")
	}
}

func validateLocalRepo(repoURL string) error {
	raw := strings.TrimSpace(repoURL)
	if raw == "" || !filepath.IsAbs(raw) {
		return errors.New("local repository path must be an absolute path")
	}
	clean := filepath.Clean(raw)
	if clean != strings.TrimRight(raw, "/") && clean != raw {
		return errors.New("local repository path must not contain relative segments")
	}
	for _, forbidden := range []string{"/", "/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/proc", "/sys", "/dev", "/boot", "/root", "/run"} {
		if clean == forbidden || strings.HasPrefix(clean, forbidden+"/") {
			return fmt.Errorf("local repository path %q is in a restricted system directory", clean)
		}
	}
	return nil
}

func validateS3Repo(repoURL string, isAdmin bool) error {
	raw := strings.TrimSpace(repoURL)
	if !strings.HasPrefix(raw, "s3:") {
		return errors.New("S3 repository URL must start with s3:")
	}
	rest := strings.TrimPrefix(raw, "s3:")
	if rest == "" {
		return errors.New("S3 repository URL is missing endpoint and bucket")
	}
	parseTarget := rest
	if !strings.HasPrefix(parseTarget, "http://") && !strings.HasPrefix(parseTarget, "https://") {
		parseTarget = "https://" + parseTarget
	}
	u, err := url.Parse(parseTarget)
	if err != nil || u.Hostname() == "" {
		return errors.New("invalid S3 repository URL")
	}
	if strings.Trim(u.Path, "/") == "" {
		return errors.New("S3 repository URL must include a bucket name")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" || host == "metadata" {
		return errors.New("S3 repository host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("S3 repository IP address is not allowed")
		}
		if !isAdmin && ip.IsPrivate() {
			return errors.New("private S3 endpoint addresses require administrator privileges")
		}
	}
	return nil
}

func validSSHIdentifier(s string, allowDots bool) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
		case allowDots && c == '.':
		default:
			return false
		}
	}
	return true
}

// parseSFTPRepo extracts user, host, port from a restic SFTP repository URL
// (e.g., "sftp:user@host:23/path" or "sftp://user@host:22/path").
func parseSFTPRepo(repoURL string) (userHost, port string, err error) {
	raw := strings.TrimSpace(repoURL)
	if strings.ContainsAny(raw, " \t\r\n'\"$`\\;&|<>\x00") {
		return "", "", fmt.Errorf("invalid characters in SFTP repository URL")
	}
	var user, hostPart, repoPath string
	switch {
	case strings.HasPrefix(raw, "sftp://"):
		u, perr := url.Parse(raw)
		if perr != nil || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
			return "", "", fmt.Errorf("invalid SFTP repository URL")
		}
		user = u.User.Username()
		hostPart = u.Hostname()
		port = u.Port()
		repoPath = u.Path
	case strings.HasPrefix(raw, "sftp:"):
		rest := strings.TrimPrefix(raw, "sftp:")
		// Split user@host[:port][:path|/path]
		at := strings.IndexByte(rest, '@')
		if at <= 0 {
			return "", "", fmt.Errorf("SFTP repository URL must include user@host")
		}
		user = rest[:at]
		afterUser := rest[at+1:]
		hostPart = afterUser
		if slash := strings.IndexByte(afterUser, '/'); slash >= 0 {
			hostPart = afterUser[:slash]
			repoPath = afterUser[slash:]
		}
		if colon := strings.IndexByte(hostPart, ':'); colon >= 0 {
			tail := hostPart[colon+1:]
			hostPart = hostPart[:colon]
			if _, perr := strconv.Atoi(tail); perr == nil {
				port = tail
			} else if repoPath == "" {
				repoPath = tail
			} else {
				port = tail
			}
		}
		if hostPart == "" {
			return "", "", fmt.Errorf("SFTP repository URL is missing host")
		}
	default:
		return "", "", fmt.Errorf("not an SFTP repository URL")
	}

	if !validSSHIdentifier(user, true) {
		return "", "", fmt.Errorf("invalid SFTP username")
	}
	if strings.HasPrefix(hostPart, "-") || (!validSSHIdentifier(hostPart, true) && net.ParseIP(hostPart) == nil) {
		return "", "", fmt.Errorf("invalid SFTP host")
	}
	if strings.Trim(repoPath, "/") == "" {
		return "", "", fmt.Errorf("SFTP repository URL must include a path")
	}
	userHost = user + "@" + hostPart
	if port != "" {
		p, perr := strconv.Atoi(port)
		if perr != nil || p < 1 || p > 65535 {
			return "", "", fmt.Errorf("invalid SFTP port %q", port)
		}
	}
	return userHost, port, nil
}

// prepareSFTPOpts writes an isolated 0600 Ed25519 private key (and optional
// known_hosts) to a temporary directory and returns `-o sftp.command=...`.
func prepareSFTPOpts(dest *models.BackupDestination) ([]string, func(), error) {
	if dest.Kind != "sftp" || dest.SSHPrivateKey == "" {
		return nil, func() {}, nil
	}
	userHost, port, err := parseSFTPRepo(dest.RepoURL)
	if err != nil {
		return nil, nil, err
	}

	tmpDir, err := os.MkdirTemp("", "cloud-console-restic-ssh-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	keyPath := filepath.Join(tmpDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte(dest.SSHPrivateKey), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
	}
	if strings.TrimSpace(dest.SSHKnownHosts) != "" {
		khPath := filepath.Join(tmpDir, "known_hosts")
		if err := os.WriteFile(khPath, []byte(dest.SSHKnownHosts+"\n"), 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
		args = append(args, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+khPath)
	} else {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=/dev/null")
	}
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, "--", userHost, "-s", "sftp")

	return []string{"-o", "sftp.command=" + strings.Join(args, " ")}, cleanup, nil
}

func mapResticError(err error, stderr string) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	code := -1
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	detail := strings.TrimSpace(stderr)
	switch code {
	case resticExitNoRepo:
		return fmt.Errorf("restic repository is not initialized")
	case resticExitLocked:
		return fmt.Errorf("restic repository is locked by another operation")
	case resticExitBadPass:
		return fmt.Errorf("incorrect restic repository password")
	case resticExitPartial:
		if detail != "" {
			return fmt.Errorf("restic partial read error: %s", detail)
		}
		return fmt.Errorf("restic partial read error")
	}
	if detail != "" {
		return fmt.Errorf("restic failed (exit %d): %s", code, detail)
	}
	return fmt.Errorf("restic failed: %w", err)
}

func (s *Service) runRestic(ctx context.Context, dest *models.BackupDestination, instanceName string, args ...string) ([]byte, error) {
	if err := ValidateDestination(dest, true); err != nil {
		return nil, err
	}
	cacheDir := s.resticCacheDir(dest.ID, instanceName)
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	sftpOpts, cleanup, err := prepareSFTPOpts(dest)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	fullArgs := append([]string{"--cache-dir", cacheDir}, sftpOpts...)
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, s.resticExecutable(), fullArgs...)
	cmd.Env = s.buildResticEnv(dest, instanceName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), mapResticError(err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (s *Service) ensureResticRepoInit(ctx context.Context, dest *models.BackupDestination, instanceName string) error {
	_, err := s.runRestic(ctx, dest, instanceName, "cat", "config")
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "not initialized") {
		if _, initErr := s.runRestic(ctx, dest, instanceName, "init"); initErr != nil {
			return fmt.Errorf("init repository: %w", initErr)
		}
		return nil
	}
	return err
}

// EnqueueResticBackup schedules a zero-temp-disk uncompressed Incus -> Restic
// streaming backup job.
func (s *Service) EnqueueResticBackup(username, instanceName string, destinationID int64, backupName string) (*models.Job, error) {
	if _, err := s.app.Incus.Instance(instanceName); err != nil {
		return nil, ErrNotFound
	}
	if _, err := s.app.DB.GetBackupDestination(destinationID); err != nil {
		return nil, fmt.Errorf("backup destination not found")
	}
	backupName = strings.TrimSpace(backupName)
	if backupName == "" {
		backupName = SuggestName(instanceName, time.Now())
	}
	if !validBackupName(backupName) {
		return nil, ErrInvalidName
	}
	payload, err := json.Marshal(resticBackupPayload{
		DestinationID: destinationID,
		Instance:      instanceName,
		BackupName:    backupName,
	})
	if err != nil {
		return nil, err
	}
	return s.app.Jobs.Enqueue(s.ownerOf(instanceName), username, JobResticBackup, instanceName, string(payload))
}

// EnqueueScheduledRestic enqueues an automatic Restic backup for instanceName
// when its owner has configured at least one Restic destination and no job is
// currently running for the instance.
func (s *Service) EnqueueScheduledRestic(instanceName string) {
	if s.app == nil || s.app.DB == nil || s.app.Jobs == nil {
		return
	}
	if jobs, err := s.app.DB.ListActiveJobs(); err == nil {
		for _, j := range jobs {
			if j.Target == instanceName {
				return
			}
		}
	}
	ownerID := s.ownerOf(instanceName)
	if ownerID <= 0 {
		return
	}
	dests, err := s.app.DB.ListBackupDestinationsScoped(models.Scope{UserID: ownerID})
	if err != nil || len(dests) == 0 {
		return
	}
	name := "auto-" + SuggestName(instanceName, time.Now())
	_, _ = s.EnqueueResticBackup("scheduler", instanceName, dests[0].ID, name)
}

func snapshotSourceInstance(snap *models.ResticSnapshot) string {
	if snap.RepoPrefix != "" {
		trimmed := strings.TrimRight(snap.RepoPrefix, "/")
		if idx := strings.LastIndexByte(trimmed, '/'); idx >= 0 && idx+1 < len(trimmed) {
			return trimmed[idx+1:]
		}
	}
	return snap.InstanceName
}

// EnqueueResticRestore schedules a streaming restore from Restic directly into
// Incus.
func (s *Service) EnqueueResticRestore(username string, snapshotRowID int64, targetInstance string) (*models.Job, error) {
	snap, err := s.app.DB.GetResticSnapshot(snapshotRowID)
	if err != nil {
		return nil, ErrNotFound
	}
	dest, err := s.app.DB.GetBackupDestination(snap.DestinationID)
	if err != nil {
		return nil, ErrNotFound
	}
	targetInstance = strings.TrimSpace(targetInstance)
	if targetInstance == "" {
		targetInstance = snap.InstanceName
	}
	sourceInstance := snapshotSourceInstance(snap)
	payload, err := json.Marshal(resticRestorePayload{
		SnapshotRowID:  snap.ID,
		DestinationID:  snap.DestinationID,
		SourceInstance: sourceInstance,
		TargetInstance: targetInstance,
		SnapshotID:     snap.SnapshotID,
	})
	if err != nil {
		return nil, err
	}
	ownerID := s.ownerOf(targetInstance)
	if ownerID == 0 {
		ownerID = dest.UserID
	}
	return s.app.Jobs.Enqueue(ownerID, username, JobResticRestore, targetInstance, string(payload))
}

// EnqueueResticPrune schedules `restic forget --prune` using the destination's
// retention policy.
func (s *Service) EnqueueResticPrune(username string, destinationID int64, instanceName string) (*models.Job, error) {
	dest, err := s.app.DB.GetBackupDestination(destinationID)
	if err != nil {
		return nil, ErrNotFound
	}
	payload, err := json.Marshal(resticMaintenancePayload{
		DestinationID: destinationID,
		Instance:      instanceName,
	})
	if err != nil {
		return nil, err
	}
	target := "dest:" + dest.Name
	if instanceName != "" {
		target = instanceName
	}
	return s.app.Jobs.Enqueue(dest.UserID, username, JobResticPrune, target, string(payload))
}

// EnqueueResticCheck schedules `restic check --read-data-subset=5%` for a
// destination.
func (s *Service) EnqueueResticCheck(username string, destinationID int64) (*models.Job, error) {
	dest, err := s.app.DB.GetBackupDestination(destinationID)
	if err != nil {
		return nil, ErrNotFound
	}
	payload, err := json.Marshal(resticMaintenancePayload{
		DestinationID: destinationID,
	})
	if err != nil {
		return nil, err
	}
	return s.app.Jobs.Enqueue(dest.UserID, username, JobResticCheck, "dest:"+dest.Name, string(payload))
}

// DeleteResticSnapshot removes one snapshot from the Restic repo (`restic forget --prune`)
// and deletes its SQLite metadata row.
func (s *Service) DeleteResticSnapshot(ctx context.Context, snapshotRowID int64) error {
	snap, err := s.app.DB.GetResticSnapshot(snapshotRowID)
	if err != nil {
		return err
	}
	dest, err := s.app.DB.GetBackupDestination(snap.DestinationID)
	if err != nil {
		return err
	}
	sourceInstance := snapshotSourceInstance(snap)
	if _, err := s.runRestic(ctx, dest, sourceInstance, "forget", snap.SnapshotID, "--prune", "--json"); err != nil {
		return err
	}
	return s.app.DB.DeleteResticSnapshot(snap.ID)
}

type resticEvent struct {
	MessageType         string  `json:"message_type"`
	PercentDone         float64 `json:"percent_done"`
	BytesDone           int64   `json:"bytes_done"`
	TotalBytes          int64   `json:"total_bytes"`
	SnapshotID          string  `json:"snapshot_id"`
	DataAdded           int64   `json:"data_added"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
}

func (s *Service) handleResticBackup(ctx context.Context, job *models.Job) error {
	var p resticBackupPayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return err
	}
	dest, err := s.app.DB.GetBackupDestination(p.DestinationID)
	if err != nil {
		return err
	}

	s.app.Jobs.Progress(job.ID, 5, "Preparing Restic repository")
	if err := s.ensureResticRepoInit(ctx, dest, p.Instance); err != nil {
		return err
	}

	s.app.Jobs.Progress(job.ID, 12, "Creating uncompressed Incus export")
	// Never feed gzip into restic: uncompressed tars preserve content-defined
	// chunk deduplication across runs and avoid single-core gzip bottlenecks.
	if err := s.app.Incus.CreateBackup(ctx, p.Instance, incusapi.InstanceBackupsPost{
		Name:                 p.BackupName,
		InstanceOnly:         true,
		OptimizedStorage:     false,
		CompressionAlgorithm: "none",
	}); err != nil {
		return fmt.Errorf("create uncompressed backup: %w", err)
	}
	defer func() {
		if derr := s.app.Incus.DeleteBackup(context.WithoutCancel(ctx), p.Instance, p.BackupName); derr != nil {
			s.app.Log.Warn("could not remove temporary incus backup", "backup", p.BackupName, "err", derr)
		}
	}()

	cacheDir := s.resticCacheDir(dest.ID, p.Instance)
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return err
	}
	sftpOpts, cleanup, err := prepareSFTPOpts(dest)
	if err != nil {
		return err
	}
	defer cleanup()

	resticCtx, cancelRestic := context.WithCancel(ctx)
	defer cancelRestic()

	args := append([]string{"--cache-dir", cacheDir}, sftpOpts...)
	args = append(args, "backup", "--stdin", "--stdin-filename", p.Instance+".tar", "--json")

	cmd := exec.CommandContext(resticCtx, s.resticExecutable(), args...)
	cmd.Env = s.buildResticEnv(dest, p.Instance)

	pr, pw := io.Pipe()
	cmd.Stdin = pr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}

	exportErrCh := make(chan error, 1)
	go func() {
		_, expErr := s.app.Incus.ExportBackupStream(ctx, p.Instance, p.BackupName, pw, nil)
		if expErr != nil {
			// Abort restic so an incomplete Incus export never produces a snapshot.
			cancelRestic()
		}
		exportErrCh <- expErr
	}()

	var summary resticEvent
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev resticEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.MessageType {
		case "status":
			pct := 20 + int(ev.PercentDone*75)
			if pct > 95 {
				pct = 95
			}
			s.app.Jobs.Progress(job.ID, pct, fmt.Sprintf("Streaming to %s (%d%%)", dest.Name, int(ev.PercentDone*100)))
		case "summary":
			summary = ev
		}
	}

	waitErr := cmd.Wait()
	expErr := <-exportErrCh
	if expErr != nil {
		return fmt.Errorf("stream incus export: %w", expErr)
	}
	if waitErr != nil {
		return mapResticError(waitErr, stderr.String())
	}
	if summary.SnapshotID == "" {
		return errors.New("restic finished without reporting a snapshot id")
	}

	_, err = s.app.DB.SaveResticSnapshot(&models.ResticSnapshot{
		DestinationID:  dest.ID,
		InstanceName:   p.Instance,
		SnapshotID:     summary.SnapshotID,
		BackupName:     p.BackupName,
		RepoPrefix:     RepoURLForInstance(dest, p.Instance),
		SizeBytes:      summary.TotalBytesProcessed,
		DataAddedBytes: summary.DataAdded,
		DurationSec:    summary.TotalDuration,
		Status:         "complete",
		CreatedBy:      job.CreatedBy,
	})
	return err
}

func (s *Service) handleResticRestore(ctx context.Context, job *models.Job) error {
	var p resticRestorePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return err
	}
	dest, err := s.app.DB.GetBackupDestination(p.DestinationID)
	if err != nil {
		return err
	}

	cacheDir := s.resticCacheDir(dest.ID, p.SourceInstance)
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return err
	}
	sftpOpts, cleanup, err := prepareSFTPOpts(dest)
	if err != nil {
		return err
	}
	defer cleanup()

	args := append([]string{"--cache-dir", cacheDir}, sftpOpts...)
	args = append(args, "dump", p.SnapshotID, p.SourceInstance+".tar")

	cmd := exec.CommandContext(ctx, s.resticExecutable(), args...)
	cmd.Env = s.buildResticEnv(dest, p.SourceInstance)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}

	dumpErrCh := make(chan error, 1)
	go func() {
		werr := cmd.Wait()
		if werr != nil {
			werr = mapResticError(werr, stderr.String())
		}
		_ = pw.CloseWithError(werr)
		dumpErrCh <- werr
	}()

	outcome, restoreErr := restoreInstance(ctx, s.app.Incus, p.TargetInstance, pr, func(pct int, msg string) {
		s.app.Jobs.Progress(job.ID, pct, msg)
	})
	_ = pr.Close()
	dumpErr := <-dumpErrCh
	if dumpErr != nil {
		return dumpErr
	}
	if restoreErr != nil {
		return restoreErr
	}
	if outcome.StartErr != nil {
		s.app.Log.Warn("restored instance did not start", "instance", p.TargetInstance, "err", outcome.StartErr)
	}
	// If the instance had been deleted before this restore, recreate its
	// console ownership record so the tenant can access the restored instance.
	if _, merr := s.app.DB.GetInstanceMeta(p.TargetInstance); merr != nil {
		ownerID := job.OwnerID
		if ownerID == 0 {
			ownerID = dest.UserID
		}
		meta := &models.InstanceMeta{
			Name:    p.TargetInstance,
			OwnerID: ownerID,
		}
		if inst, ierr := s.app.Incus.Instance(p.TargetInstance); ierr == nil && inst != nil {
			meta.Kind = string(inst.Type)
		}
		_ = s.app.DB.SaveInstanceMeta(meta)
	}
	if s.notifier != nil {
		s.notifier.InstanceChanged(ctx, p.TargetInstance)
	}
	return nil
}

func (s *Service) handleResticPrune(ctx context.Context, job *models.Job) error {
	var p resticMaintenancePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return err
	}
	dest, err := s.app.DB.GetBackupDestination(p.DestinationID)
	if err != nil {
		return err
	}

	instances := []string{p.Instance}
	if p.Instance == "" {
		instances, err = s.app.DB.DistinctInstancesForDestination(dest.ID)
		if err != nil {
			return err
		}
	}
	if len(instances) == 0 {
		return nil
	}

	for i, inst := range instances {
		s.app.Jobs.Progress(job.ID, 10+(i*80)/len(instances), "Pruning "+inst)
		args := []string{
			"forget",
			"--keep-last", strconv.Itoa(maxInt(dest.KeepLast, 1)),
			"--keep-daily", strconv.Itoa(maxInt(dest.KeepDaily, 0)),
			"--keep-weekly", strconv.Itoa(maxInt(dest.KeepWeekly, 0)),
			"--prune",
			"--json",
		}
		if _, err := s.runRestic(ctx, dest, inst, args...); err != nil {
			return err
		}
		// Refresh remaining snapshots so SQLite stays in sync with the repo.
		out, err := s.runRestic(ctx, dest, inst, "snapshots", "--json")
		if err == nil {
			var snaps []struct {
				ID      string `json:"id"`
				ShortID string `json:"short_id"`
			}
			if json.Unmarshal(out, &snaps) == nil {
				keep := make(map[string]bool, len(snaps)*2)
				for _, sn := range snaps {
					if sn.ID != "" {
						keep[sn.ID] = true
					}
					if sn.ShortID != "" {
						keep[sn.ShortID] = true
					}
				}
				_ = s.app.DB.SyncResticSnapshots(dest.ID, inst, keep)
			}
		}
	}
	return nil
}

func (s *Service) handleResticCheck(ctx context.Context, job *models.Job) error {
	var p resticMaintenancePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return err
	}
	dest, err := s.app.DB.GetBackupDestination(p.DestinationID)
	if err != nil {
		return err
	}
	instances, err := s.app.DB.DistinctInstancesForDestination(dest.ID)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		_ = s.app.DB.UpdateBackupDestinationCheck(dest.ID, "ok (no snapshots yet)", time.Now())
		return nil
	}
	for i, inst := range instances {
		s.app.Jobs.Progress(job.ID, 10+(i*85)/len(instances), "Checking "+inst)
		if _, err := s.runRestic(ctx, dest, inst, "check", "--read-data-subset=5%", "--json"); err != nil {
			_ = s.app.DB.UpdateBackupDestinationCheck(dest.ID, "failed: "+err.Error(), time.Now())
			return err
		}
	}
	_ = s.app.DB.UpdateBackupDestinationCheck(dest.ID, "ok", time.Now())
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
