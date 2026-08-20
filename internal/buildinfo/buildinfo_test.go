package buildinfo

import "testing"

func TestExecutorImageTracksBuildVersion(t *testing.T) {
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "")
	previous := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = previous })
	if got := ExecutorImage(); got != "ghcr.io/rockingsoft/lamplight:1.2.3" {
		t.Fatalf("ExecutorImage = %q", got)
	}
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "example.test/lamplight:development")
	if got := ExecutorImage(); got != "example.test/lamplight:development" {
		t.Fatalf("override = %q", got)
	}
}
