package buildinfo

import "os"

// Version is replaced by GoReleaser. Development builds may override the
// executor image explicitly without adding project configuration.
var Version = "dev"

func ExecutorImage() string {
	if image := os.Getenv("LAMPLIGHT_RUNNER_IMAGE"); image != "" {
		return image
	}
	tag := Version
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	return "ghcr.io/rockingsoft/lamplight:" + tag
}
