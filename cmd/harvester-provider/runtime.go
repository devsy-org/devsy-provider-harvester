package main

import (
	_ "embed"
	"regexp"
	"strings"
)

const rootfulPodmanCommand = "/usr/local/bin/devsy-podman-rootful"

//go:embed profiles/podman-rootful.sh
var podmanRootfulWrapper string

var guestWordPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func podmanWrapperScript() string {
	return strings.ReplaceAll(podmanRootfulWrapper, "@ROOTFUL_PODMAN_WRAPPER@", rootfulPodmanCommand)
}

func defaultRuntimeCommand(c config) string {
	if profile, ok := bootstrapProfiles()[c.CloudInitProfile]; ok && profile.Runtime == containerRuntime(c) {
		return profile.Command
	}
	return containerRuntime(c)
}

func runtimeCommand(c config) string {
	if c.ContainerCommand != "" {
		return c.ContainerCommand
	}
	return defaultRuntimeCommand(c)
}

func quoteGuestWord(word string) string {
	if guestWordPattern.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", "'\\''") + "'"
}

func indentGuestScript(width int, script string) string {
	prefix := strings.Repeat(" ", width)
	return prefix + strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n"+prefix)
}
