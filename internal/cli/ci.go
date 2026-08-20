package cli

import "strings"

var standardCIEnvironmentVariables = []string{
	"CI",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"BUILDKITE",
	"TF_BUILD",
	"CIRCLECI",
	"TRAVIS",
	"JENKINS_URL",
	"BITBUCKET_BUILD_NUMBER",
	"TEAMCITY_VERSION",
	"DRONE",
}

func isCIEnvironment(getenv func(string) string) bool {
	for _, name := range standardCIEnvironmentVariables {
		value := strings.TrimSpace(strings.ToLower(getenv(name)))
		if value != "" && value != "0" && value != "false" {
			return true
		}
	}
	return false
}
