//go:build integration

// Package integration runs tagbrr as the shipped image against a mock
// qBittorrent, over a real docker network. Nothing upstream is contacted:
// the subject builds from the repository, the mock builds from its own
// source into scratch, so the suite works offline (the subject's golang
// base layer comes from the local daemon cache after the first ever run).
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// label marks everything this suite creates, so a sweep removes
	// exactly ours and nothing else on the host.
	label = "tagbrr-test"

	subjectImage = "tagbrr:integration"
	mockImage    = "tagbrr-mockqbit:integration"
	network      = "tagbrr-integration"
)

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// docker runs one docker command and returns its combined output.
func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// buildSubject builds tagbrr from the shipped Dockerfile — the artifact
// people actually run, not a test-only variant that could drift from it.
func buildSubject(root string) error {
	_, err := docker("build", "-q", "--label", label+"=1", "-t", subjectImage, root)
	return err
}

// buildMock builds mockqbit from its own source into a scratch image.
// FROM scratch: nothing is pulled, so the suite works offline.
func buildMock(root string) error {
	dir, err := os.MkdirTemp("", "tagbrr-mockqbit")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o",
		filepath.Join(dir, "mockqbit"), "./test/integration/mockqbit")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	dockerfile := "FROM scratch\nCOPY mockqbit /mockqbit\nENTRYPOINT [\"/mockqbit\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}
	_, err = docker("build", "-q", "--label", label+"=1", "-t", mockImage, dir)
	return err
}

// sweep removes every container and network this suite ever created —
// before a run as well as after, so a killed run does not poison the next.
func sweep() {
	if out, _ := docker("ps", "-aq", "--filter", "label="+label); out != "" {
		docker(append([]string{"rm", "-f"}, strings.Fields(out)...)...)
	}
	docker("network", "rm", network)
}

// Setup builds both images and creates the network every scenario shares.
func Setup() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	if err := buildSubject(root); err != nil {
		return "", fmt.Errorf("building %s: %w", subjectImage, err)
	}
	if err := buildMock(root); err != nil {
		return "", fmt.Errorf("building %s: %w", mockImage, err)
	}
	sweep()
	if _, err := docker("network", "create", "--label", label+"=1", network); err != nil {
		return "", err
	}
	return root, nil
}

// Teardown removes what Setup and the scenarios started. The images are
// left: rebuilt next run, and removing them would throw away the layer
// cache that makes a rerun quick.
func Teardown() {
	sweep()
}
