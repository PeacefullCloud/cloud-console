package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/backups"
	"github.com/peaceful/cloud-console/internal/domains"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/monitoring"
	"github.com/peaceful/cloud-console/internal/snapshots"
)

// baseData carries the fields every layout-rendered page needs.
type baseData struct {
	Title         string
	Nav           string
	CSRF          string
	Query         string
	User          *models.User
	Version       string
	IncusSocket   string
	InstanceCount int
	Jobs          []models.Job

	Error     string
	Notice    string
	BodyClass string
}

// --- dashboard ------------------------------------------------------------

type dashboardData struct {
	baseData
	Instances []instances.Instance
	Totals    instances.Totals
	Host      *monitoring.Host
	Activity  []models.Activity
	S3Enabled bool
}

// --- instances list -------------------------------------------------------

type instancesData struct {
	baseData
	Instances []instances.Instance
	Groups    []poolGroup
	Running   int
	Stopped   int
}

type poolGroup struct {
	Name      string
	Driver    string
	Instances []instances.Instance
}

// --- create wizard --------------------------------------------------------

type createData struct {
	baseData
	Platforms    []platformInfo
	Images       []incus.Image
	Sizes        []Size
	Pools        []poolInfo
	DefaultImage string
	Form         createForm
}

type platformInfo struct {
	ID          string
	Label       string
	Description string
	Count       int
}

type poolInfo struct {
	Name   string
	Driver string
}

type createForm struct {
	Name         string
	Domain       string
	Notes        string
	Platform     string
	Image        string
	Kind         string // container | virtual-machine
	RootPassword string
	SSHKey       string
	Size         string
	StoragePool  string
	CPU          int
	MemoryMB     int
	DiskGB       int
}

// --- instance detail ------------------------------------------------------

type tabItem struct {
	ID    string
	Label string
}

type windowOption struct {
	ID     string
	Label  string
	Active bool
}

type instanceData struct {
	baseData
	Inst *instances.Instance

	Tab    string
	Tabs   []tabItem
	Window string

	Windows []windowOption

	WindowLabel    string
	SampleInterval int
	CPUSeries      monitoring.Series
	MemSeries      monitoring.Series

	Snapshots []snapshots.Snapshot
	Backups   []backups.Backup
	Domains   []domains.Record
	Activity  []models.Activity
	Disks     []diskView
	Networks  []networkView
	Images    []incus.Image
	Sizes     []Size

	// ConsoleLog holds the tail of the instance's console output.
	ConsoleLog    []string
	ConsoleLogErr string

	S3Enabled    bool
	S3Bucket     string
	CaddyEnabled bool

	SuggestedSnapshot string
	SuggestedBackup   string
}

type diskView struct {
	Name   string
	Type   string
	Pool   string
	Source string
	Path   string
}

type networkView struct {
	Name    string
	Type    string
	Network string
	MAC     string
	State   string
}

func instanceTabs() []tabItem {
	return []tabItem{
		{"overview", "Overview"},
		{"metrics", "Metrics"},
		{"snapshots", "Snapshots"},
		{"storage", "Storage"},
		{"networking", "Networking"},
		{"domains", "Domains"},
		{"backups", "Backups"},
		{"history", "History"},
		{"settings", "Settings"},
	}
}

func windowOptions(active string) []windowOption {
	options := []windowOption{
		{"hour", "1 hour", false},
		{"six", "6 hours", false},
		{"day", "1 day", false},
		{"week", "1 week", false},
		{"month", "1 month", false},
	}
	for i := range options {
		options[i].Active = options[i].ID == active
	}
	return options
}

// windowDuration maps a window key to its length and label.
func windowDuration(key string) (window time.Duration, label string) {
	switch key {
	case "six":
		return 6 * monitoring.WindowHour, "6 hours"
	case "day":
		return monitoring.WindowDay, "24 hours"
	case "week":
		return monitoring.WindowWeek, "7 days"
	case "month":
		return monitoring.WindowMonth, "30 days"
	default:
		return monitoring.WindowHour, "hour"
	}
}

// --- other pages ----------------------------------------------------------

type snapshotsData struct {
	baseData
	Snapshots []snapshots.Snapshot
}

type backupsData struct {
	baseData
	Backups    []backups.Backup
	S3Enabled  bool
	S3Bucket   string
	TotalBytes int64
}

type domainsData struct {
	baseData
	Domains      []domains.Record
	CaddyPreview string
	CaddyEnabled bool
	PublicIP     string
}

type storageData struct {
	baseData
	Host *hostView
}

// hostView is the storage page's view of the host: pools carry the instances
// that live on them.
type hostView struct {
	*monitoring.Host
	Pools []poolView
}

type poolView struct {
	Name      string
	Driver    string
	Used      int64
	Total     int64
	Free      int64
	Instances []instances.Instance
}

type networkInfo struct {
	Name        string
	Type        string
	Managed     string
	Address     string
	UsedBy      int
	Description string
}

type networkingData struct {
	baseData
	Networks     []networkInfo
	Instances    []instances.Instance
	CaddyEnabled bool
	CaddyPath    string
	CaddyAdmin   string
}

type activityData struct {
	baseData
	Activity []models.Activity
	Total    int
	Page     int
	Pages    int
	Limit    int

	// Jobs shadows baseData.Jobs so the activity page can show a longer history.
	Jobs []models.Job
}

type settingsData struct {
	baseData
	Host    *monitoring.Host
	Users   []models.User
	Preview string

	S3Configured      bool
	S3Bucket          string
	S3Endpoint        string
	S3Region          string
	S3Prefix          string
	S3AccessKeyMasked string

	CaddyEnabled   bool
	CaddyPath      string
	CaddyAdmin     string
	CaddyReloadCmd string
	CaddyPreview   string

	SessionCount int
	SessionTTL   int

	// Two-factor state for the signed-in user, plus a pending setup secret
	// shown once right after generation.
	TOTPEnabled     bool
	TOTPSetupSecret string
	TOTPSetupURL    string

	MetricsInterval  int
	MetricsRetention int
	JobWorkers       int

	IncusProject string
	RunningCount int
}

type jobsData struct {
	Jobs []models.Job
}

type loginData struct {
	Title    string
	CSRF     string
	Error    string
	Notice   string
	Next     string
	Username string
}

// totpData is the second login step: the password was correct and the user
// proves possession of the authenticator app.
type totpData struct {
	Title     string
	CSRF      string
	Error     string
	Challenge string
	Username  string
	Next      string
}

// --- size catalog ---------------------------------------------------------

// Size is a preset plan shown in the create wizard.
type Size struct {
	ID       string
	Label    string
	CPU      int
	MemoryMB int
	DiskGB   int
}

// MemoryBytes returns the plan's memory in bytes.
func (s Size) MemoryBytes() int64 { return int64(s.MemoryMB) << 20 }

// Value is the composite form value submitted by the wizard.
func (s Size) Value() string {
	return fmt.Sprintf("%d|%d|%d", s.CPU, s.MemoryMB, s.DiskGB)
}

// DefaultSizes mirrors the small, predictable plan list from the V1 plan.
func DefaultSizes() []Size {
	return []Size{
		{ID: "nano", Label: "Nano — smallest", CPU: 1, MemoryMB: 512, DiskGB: 10},
		{ID: "micro", Label: "Micro — small site", CPU: 1, MemoryMB: 1024, DiskGB: 20},
		{ID: "small", Label: "Small — most sites", CPU: 2, MemoryMB: 2048, DiskGB: 40},
		{ID: "medium", Label: "Medium — WordPress", CPU: 2, MemoryMB: 4096, DiskGB: 60},
		{ID: "large", Label: "Large — busy app", CPU: 4, MemoryMB: 8192, DiskGB: 120},
		{ID: "xlarge", Label: "Extra large", CPU: 8, MemoryMB: 16384, DiskGB: 240},
	}
}

// ParseSizeValue decodes "cpu|memMB|diskGB".
func ParseSizeValue(value string) (cpu, memoryMB, diskGB int, ok bool) {
	parts := strings.Split(value, "|")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}

	cpu, err1 := strconv.Atoi(parts[0])
	memoryMB, err2 := strconv.Atoi(parts[1])
	diskGB, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return cpu, memoryMB, diskGB, true
}
