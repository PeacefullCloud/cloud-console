package web

import "testing"

func TestTitleActionForceStop(t *testing.T) {
	if got := titleAction("force-stop"); got != "Force stop instance" {
		t.Errorf("titleAction(force-stop) = %q", got)
	}
	if got := titleAction("stop"); got != "Stop instance" {
		t.Errorf("titleAction(stop) = %q", got)
	}
}
