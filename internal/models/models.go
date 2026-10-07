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
	CreatedAt    time.Time
	LastLoginAt  *time.Time

	// TOTP two-factor state. Secret is stored from setup start; Enabled
	// flips on only after a code from the authenticator app is confirmed.
	TOTPSecret  string
	TOTPEnabled bool
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

// Activity is a single audit-log entry.
type Activity struct {
	ID       int64
	TS       time.Time
	Username string
	Action   string
	Target   string
	Detail   string
	Status   string // ok | error
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

// Job tracks a long-running operation so the UI can follow progress.
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
	CreatedAt time.Time
	StartedAt *time.Time
	EndedAt   *time.Time
}

// Setting is a simple key/value configuration entry.
type Setting struct {
	Key   string
	Value string
}
