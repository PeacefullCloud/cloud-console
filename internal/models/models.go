// Package models contains the console's own data structures.
//
// Incus remains the source of truth for infrastructure (CPU, RAM, disks, IPs,
// running state, snapshots, storage). These structures only describe the
// console-specific metadata stored in SQLite.
package models

import "time"

// User is a console operator.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string // admin | operator | viewer
	AuthMethod   string // either | password | sso — which sign-in paths work
	CreatedAt    time.Time
	LastLoginAt  *time.Time

	// MaxInstances is the per-user instance quota. 0 means unlimited; the
	// host-wide limit still applies.
	MaxInstances int

	// TOTP two-factor state. Secret is stored from setup start; Enabled
	// flips on only after a code from the authenticator app is confirmed.
	TOTPSecret  string
	TOTPEnabled bool
}

// SSHKey is a tenant's stored SSH public key. Only the public half is kept:
// a keypair generated in the console returns its private key to the user once
// and never persists it. OwnerID is the only tenant that may use this key.
type SSHKey struct {
	ID          int64
	OwnerID     int64
	Name        string
	PublicKey   string // authorized_keys line: "ssh-ed25519 AAAA... comment"
	Fingerprint string // e.g. "SHA256:AbC..."
	CreatedAt   time.Time
}

// InstanceMeta is the console-side record for an Incus instance. Resource limits
// and live state are read from Incus, not from here.
type InstanceMeta struct {
	ID          int64
	Name        string
	Image       string // incus image alias, e.g. images:ubuntu/24.04
	ImageLabel  string // friendly label, e.g. "Ubuntu 24.04"
	Kind        string // container | virtual-machine
	CPU         int    // recorded at creation time
	MemoryMB    int    // recorded at creation time
	DiskGB      int    // recorded at creation time
	StoragePool string
	OwnerID     int64
	OwnerName   string
	PrimaryHost string // primary domain, "" when none
	Notes       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IDName returns the instance name for logging/URL purposes.
func (i InstanceMeta) IDName() string { return i.Name }

// Domain is a hostname routed to an instance through the host Caddy.
type Domain struct {
	ID           int64
	InstanceName string
	Domain       string
	Port         int // container-side port, default 80
	CreatedAt    time.Time
}

// SnapshotMeta is console bookkeeping for an Incus snapshot. The snapshot itself
// lives in Incus.
type SnapshotMeta struct {
	ID           int64
	InstanceName string
	Name         string
	Note         string
	CreatedBy    string
	CreatedAt    time.Time
}

// Backup records an exported instance backup.
type Backup struct {
	ID           int64
	InstanceName string
	Name         string
	Target       string // local | s3
	S3Key        string
	SizeBytes    int64
	Status       string // pending | running | complete | failed
	Error        string
	CreatedBy    string
	CreatedAt    time.Time
}

// Activity is a single audit-log entry. OwnerID is the tenant the entry is
// about: the owner of the target instance, or the acting user otherwise.
type Activity struct {
	ID       int64
	TS       time.Time
	Username string
	Action   string
	Target   string
	Detail   string
	Status   string // ok | error
	OwnerID  int64
}

// Metric is one monitoring sample for an instance.
type Metric struct {
	ID           int64
	InstanceName string
	TS           time.Time
	CPUPct       float64
	MemUsed      int64
	MemTotal     int64
	DiskUsed     int64
	DiskTotal    int64
	NetRxBytes   int64
	NetTxBytes   int64
}

// Job tracks a long-running operation so the UI can follow progress. OwnerID
// is the tenant the job belongs to (the target instance's owner, or the owner
// chosen when creating an instance).
type Job struct {
	ID        string
	Kind      string
	Target    string
	Payload   string
	Status    string // queued | running | done | failed
	Progress  int
	Message   string
	Error     string
	CreatedBy string
	OwnerID   int64
	CreatedAt time.Time
	StartedAt *time.Time
	EndedAt   *time.Time
}

// Scope limits queries to the data one console user may see. Admins see
// everything, including instances created outside the console.
type Scope struct {
	UserID int64
	Admin  bool
}

// All reports whether the scope may see every owner's data.
func (s Scope) All() bool { return s.Admin }

// CanAccessOwner reports whether the scope may see data owned by ownerID.
// Unowned records (ownerID 0, including instances created outside the console)
// are visible to administrators only.
func (s Scope) CanAccessOwner(ownerID int64) bool {
	return s.Admin || (ownerID > 0 && ownerID == s.UserID)
}

// Setting is a simple key/value configuration entry.
type Setting struct {
	Key   string
	Value string
}

// SSOProvider is an OpenID Connect identity provider configured in Settings.
// ClientSecret is always encrypted at rest; View carries the safe subset.
type SSOProvider struct {
	ID           int64
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string // encrypted when stored
	ButtonLabel  string
	DefaultRole  string // admin | operator | viewer for JIT-provisioned users
	RequireMFA   bool   // require an MFA marker in the token's amr claim
	// Provisioning is open | domains | link_only; AllowedDomains is a
	// comma-separated list used by the domains mode.
	Provisioning   string
	AllowedDomains string
	Enabled        bool
	CreatedAt      time.Time
}

// UserIdentity links a console user to their subject at a provider.
type UserIdentity struct {
	UserID     int64
	ProviderID int64
	Subject    string
	CreatedAt  time.Time
}
