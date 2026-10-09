package backups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/incus"
)

// fakeHost models just enough of Incus: named instances with a status, plus
// switchable failures.
type fakeHost struct {
	instances map[string]string // name -> status
	data      map[string]string // name -> which content it holds
	calls     []string

	failImport, failStart, failStop string
	failDelete                      bool
	stopNeedsForce                  bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{instances: map[string]string{}, data: map[string]string{}}
}

func (f *fakeHost) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeHost) Instance(name string) (*incusapi.Instance, error) {
	status, ok := f.instances[name]
	if !ok {
		return nil, errors.New("Instance not found")
	}
	return &incusapi.Instance{Name: name, Status: status}, nil
}

func (f *fakeHost) ImportBackup(_ context.Context, name string, r io.Reader, _ string) error {
	f.log("import %s", name)
	if f.failImport != "" {
		return errors.New(f.failImport)
	}
	if _, taken := f.instances[name]; taken {
		return errors.New("name taken")
	}
	body, _ := io.ReadAll(r)
	f.instances[name], f.data[name] = "Stopped", string(body)
	return nil
}

func (f *fakeHost) Stop(_ context.Context, name string) error {
	f.log("stop %s", name)
	if f.failStop != "" {
		return errors.New(f.failStop)
	}
	if f.stopNeedsForce {
		return incus.ErrNotShutDown
	}
	f.instances[name] = "Stopped"
	return nil
}

func (f *fakeHost) ForceStop(_ context.Context, name string) error {
	f.log("force-stop %s", name)
	if _, ok := f.instances[name]; ok {
		f.instances[name] = "Stopped"
	}
	return nil
}

func (f *fakeHost) Start(_ context.Context, name string) error {
	f.log("start %s", name)
	if f.failStart == name || (f.failStart == "*" && f.data[name] == "backup") {
		return errors.New("boot failure")
	}
	f.instances[name] = "Running"
	return nil
}

func (f *fakeHost) RenameInstance(_ context.Context, from, to string) error {
	f.log("rename %s -> %s", from, to)
	if f.instances[from] == "Running" {
		return errors.New("instance is running")
	}
	f.instances[to], f.data[to] = f.instances[from], f.data[from]
	delete(f.instances, from)
	delete(f.data, from)
	return nil
}

func (f *fakeHost) DeleteInstance(_ context.Context, name string) error {
	f.log("delete %s", name)
	if f.failDelete {
		return errors.New("delete failed")
	}
	delete(f.instances, name)
	delete(f.data, name)
	return nil
}

func (f *fakeHost) names() []string {
	var out []string
	for n := range f.instances {
		out = append(out, n)
	}
	return out
}

func run(t *testing.T, f *fakeHost) (restoreOutcome, error) {
	t.Helper()
	return restoreInstance(context.Background(), f, "web", strings.NewReader("backup"), func(int, string) {})
}

func (f *fakeHost) requireOnly(t *testing.T, name, wantStatus, wantData string) {
	t.Helper()
	if len(f.instances) != 1 || f.instances[name] != wantStatus || f.data[name] != wantData {
		t.Fatalf("instances = %v data = %v, want only %s (%s) holding %q\ncalls: %v",
			f.instances, f.data, name, wantStatus, wantData, f.calls)
	}
}

func TestRestoreReplacesRunningInstance(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"

	out, err := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Leftover != "" || out.StartErr != nil {
		t.Fatalf("unexpected outcome %+v", out)
	}
	f.requireOnly(t, "web", "Running", "backup")
}

func TestRestoreFreshInstanceWhenNoneExists(t *testing.T) {
	f := newFakeHost()
	if _, err := run(t, f); err != nil {
		t.Fatal(err)
	}
	f.requireOnly(t, "web", "Running", "backup")
}

func TestRestoreBadArchiveLeavesLiveInstanceUntouched(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"
	f.failImport = "corrupt archive"

	if _, err := run(t, f); err == nil || !strings.Contains(err.Error(), "corrupt archive") {
		t.Fatalf("want import error, got %v", err)
	}
	f.requireOnly(t, "web", "Running", "live")
	for _, c := range f.calls {
		if strings.HasPrefix(c, "stop") || strings.HasPrefix(c, "rename") || strings.HasPrefix(c, "delete web") {
			t.Fatalf("live instance was touched: %v", f.calls)
		}
	}
}

func TestRestoreRollsBackWhenRestoredInstanceWillNotStart(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"
	f.failStart = "*"

	if _, err := run(t, f); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("want start failure, got %v", err)
	}
	f.requireOnly(t, "web", "Running", "live")
}

// refuseRename fails renames whose source starts with fromPrefix (when
// non-empty) or whose destination starts with toPrefix.
type refuseRename struct {
	*fakeHost
	fromPrefix, toPrefix string
}

func (p *refuseRename) RenameInstance(ctx context.Context, from, to string) error {
	if (p.fromPrefix != "" && strings.HasPrefix(from, p.fromPrefix)) ||
		(p.toPrefix != "" && strings.HasPrefix(to, p.toPrefix)) {
		p.log("rename %s -> %s (refused)", from, to)
		return errors.New("rename refused")
	}
	return p.fakeHost.RenameInstance(ctx, from, to)
}

func TestRestoreRollsBackWhenFinalRenameFails(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"

	host := &refuseRename{fakeHost: f, fromPrefix: "console-restore-"}
	_, err := restoreInstance(context.Background(), host, "web", strings.NewReader("backup"), func(int, string) {})
	if err == nil {
		t.Fatal("want an error")
	}
	// The original is back under its own name and running; the import is gone.
	f.requireOnly(t, "web", "Running", "live")
}

func TestRestoreFirstRenameFailureRestartsOriginal(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"

	host := &refuseRename{fakeHost: f, toPrefix: "console-old-"}
	_, err := restoreInstance(context.Background(), host, "web", strings.NewReader("backup"), func(int, string) {})
	if err == nil {
		t.Fatal("want an error")
	}
	f.requireOnly(t, "web", "Running", "live")
}

func TestRestoreForcesStopOnlyAfterCleanStopIgnored(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"
	f.stopNeedsForce = true

	if _, err := run(t, f); err != nil {
		t.Fatal(err)
	}
	f.requireOnly(t, "web", "Running", "backup")

	stopIdx, forceIdx := -1, -1
	for i, c := range f.calls {
		if c == "stop web" {
			stopIdx = i
		}
		if c == "force-stop web" && forceIdx == -1 {
			forceIdx = i
		}
	}
	if stopIdx == -1 || forceIdx < stopIdx {
		t.Fatalf("expected a clean stop attempt before the forced one: %v", f.calls)
	}
}

func TestRestoreStopFailureAbortsWithoutChanges(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Running", "live"
	f.failStop = "incus unavailable"

	if _, err := run(t, f); err == nil {
		t.Fatal("want an error")
	}
	f.requireOnly(t, "web", "Running", "live")
}

func TestRestoreReportsLeftoverWhenOldCopyCannotBeDeleted(t *testing.T) {
	f := newFakeHost()
	f.instances["web"], f.data["web"] = "Stopped", "live"
	f.failDelete = true

	out, err := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Leftover, "console-old-") {
		t.Fatalf("Leftover = %q, want the aside name", out.Leftover)
	}
	if f.data["web"] != "backup" {
		t.Fatalf("web holds %q", f.data["web"])
	}
}

func TestRestoreWithoutPriorInstanceKeepsUnbootableRestore(t *testing.T) {
	f := newFakeHost()
	f.failStart = "web"

	out, err := run(t, f)
	if err != nil {
		t.Fatalf("nothing to roll back to, so the restore stands: %v", err)
	}
	if out.StartErr == nil {
		t.Fatal("start failure should be reported")
	}
	f.requireOnly(t, "web", "Stopped", "backup")
}
