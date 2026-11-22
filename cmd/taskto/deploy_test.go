package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The install (the image, the Compose file, the proxy config and the install guide) is
// what people run in production, so it must never turn on development-only switches.
// These tests read the files themselves: a change that slips one in fails CI.

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Plain-http cookies are for development on localhost: the image and the Compose file
// never set them.
func TestInstallKeepsSecureCookies(t *testing.T) {
	for _, name := range []string{"Dockerfile", "compose.yaml"} {
		if strings.Contains(strings.ToLower(readRepoFile(t, name)), "insecure") {
			t.Errorf("%s mentions insecure cookies", name)
		}
	}
}

// Agents never run with root or sudo (docs/agent-safety.md): the install never sets the
// development override that lets them, nor local-unsafe CLI workers.
func TestInstallKeepsAgentsUnprivileged(t *testing.T) {
	for _, name := range []string{"Dockerfile", "compose.yaml", "deploy/Caddyfile", "deploy/cli-workers.sh"} {
		if strings.Contains(strings.ToLower(readRepoFile(t, name)), "unsafe") {
			t.Errorf("%s mentions an unsafe development switch", name)
		}
	}
	// The server, whose user the sidecar runs as: no capabilities, no way to gain any.
	compose := readRepoFile(t, "compose.yaml")
	i, j := strings.Index(compose, "\n  taskto:\n"), strings.Index(compose, "\n  cli-worker:\n")
	if i < 0 || j < i {
		t.Fatal("no taskto service before the cli-worker one")
	}
	service := compose[i:j]
	for _, want := range []string{"cap_drop: [ALL]", `security_opt: ["no-new-privileges:true"]`} {
		if !strings.Contains(service, want) {
			t.Errorf("the taskto service doesn't have %q", want)
		}
	}
	if strings.Contains(service, "privileged") || strings.Contains(service, "user:") {
		t.Error("the taskto service changes the image's user or privileges")
	}
}

func TestImageIsPinnedAndUnprivileged(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")
	from := regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?(\S+)`)
	stages := from.FindAllStringSubmatch(dockerfile, -1)
	if len(stages) == 0 {
		t.Fatal("no FROM in the Dockerfile")
	}
	for _, s := range stages {
		if !regexp.MustCompile(`^[^@\s]+:[^@\s]+@sha256:[0-9a-f]{64}$`).MatchString(s[1]) {
			t.Errorf("base image %s isn't pinned by tag and digest", s[1])
		}
	}
	for _, want := range []string{"\nUSER 65532:65532\n", "TASKTO_AGENT=true", "TASKTO_DATA_DIR=/data", "\nVOLUME /data\n"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the Dockerfile doesn't have %q", strings.TrimSpace(want))
		}
	}

	for _, name := range []string{"compose.yaml", "deploy/backup-host/compose.yaml"} {
		for _, m := range regexp.MustCompile(`(?m)^\s+image:\s*(\S+)`).FindAllStringSubmatch(readRepoFile(t, name), -1) {
			// Built from this repository's Dockerfile, or published by its images workflow
			// under TASKTO_IMAGE_PREFIX with immutable tags (.github/scripts/images.sh).
			ours := strings.TrimPrefix(m[1], "${TASKTO_IMAGE_PREFIX:-}")
			if built := strings.SplitN(ours, ":", 2)[0]; slices.Contains([]string{"taskto", "taskto-cli-worker", "taskto-backup"}, built) {
				if name == "compose.yaml" && ours == m[1] {
					t.Errorf("compose.yaml: image %s can't be pointed at the published images (TASKTO_IMAGE_PREFIX)", m[1])
				}
				continue
			}
			if !strings.Contains(m[1], "@sha256:") {
				t.Errorf("%s: image %s isn't pinned by digest", name, m[1])
			}
		}
	}
}

// Backups the server can't delete (docs/install.md): the server's side only adds, the
// backup host's REST server is append-only whatever its settings, and the prune job keeps
// a time window, the one rule forged snapshots can't game.
func TestBackupsAreAppendOnly(t *testing.T) {
	compose := readRepoFile(t, "compose.yaml")
	for _, want := range []string{"target: backup", "--stdin-from-command", "-uncompressed", "RESTIC_PASSWORD_FILE", "RESTIC_REPOSITORY_FILE"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml's backup service doesn't have %q", want)
		}
	}
	// The nightly schedule is the same image and the same mounts, with taskto as its program.
	i := strings.Index(compose, "\n  backup-schedule:\n")
	if i < 0 {
		t.Fatal("compose.yaml has no backup-schedule service")
	}
	schedule := compose[i:]
	if j := strings.Index(schedule[1:], "\n\n"); j > 0 {
		schedule = schedule[:j+1]
	}
	for _, want := range []string{"<<: *backup", "profiles: [backup-schedule]", "restart: unless-stopped", "entrypoint: [taskto, backup-schedule]", "<<: *backup-env", "TASKTO_BACKUP_AT:"} {
		if !strings.Contains(schedule, want) {
			t.Errorf("compose.yaml's backup-schedule service doesn't have %q", want)
		}
	}
	// The server never prunes, and never holds the repository's secrets in the file.
	for _, banned := range []string{"forget", "--prune", "RESTIC_PASSWORD:", "RESTIC_REPOSITORY:"} {
		if strings.Contains(compose, banned) {
			t.Errorf("compose.yaml has %q: the server's side only adds snapshots, and its secrets are files", banned)
		}
	}

	host := readRepoFile(t, "deploy/backup-host/compose.yaml")
	if m := regexp.MustCompile(`(?m)^\s+OPTIONS:\s*(.*)$`).FindStringSubmatch(host); m == nil || !strings.HasPrefix(m[1], "--append-only --private-repos") {
		t.Errorf("the backup host's rest-server OPTIONS = %q, want --append-only --private-repos first, fixed", m)
	}
	if !strings.Contains(host, "${TASKTO_BACKUP_ADDR:?") {
		t.Error("the backup host publishes rest-server without requiring TASKTO_BACKUP_ADDR")
	}
	if !strings.Contains(host, "--keep-within=") {
		t.Error("the backup host's prune job doesn't forget by --keep-within")
	}
	for _, unsafe := range []string{"--keep-last", "--keep-hourly", "--keep-daily", "--keep-weekly", "--keep-monthly", "--keep-yearly", "--keep-tag"} {
		if strings.Contains(host, unsafe) {
			t.Errorf("the backup host's prune job uses %s, which forged snapshots can game", unsafe)
		}
	}
}

// The CLI worker's image installs the CLIs pinned in deploy/cli-workers.json, each checked
// against its sha256; the drivers' fixtures were recorded from those versions; and the
// worker runs with only the capabilities it needs, leaving none to the CLIs.
func TestCLIWorkerIsPinnedAndHardened(t *testing.T) {
	var manifest struct {
		CLIs map[string]struct {
			Version   string `json:"version"`
			Platforms map[string]struct {
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
			} `json:"platforms"`
		} `json:"clis"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, "deploy/cli-workers.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	sum := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, cli := range []string{"claude", "codex", "grok"} {
		c, ok := manifest.CLIs[cli]
		if !ok || !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(c.Version) {
			t.Errorf("%s: no version in the manifest", cli)
			continue
		}
		for _, arch := range []string{"amd64", "arm64"} {
			p := c.Platforms[arch]
			if !strings.HasPrefix(p.URL, "https://") || !strings.Contains(p.URL, c.Version) || !sum.MatchString(p.SHA256) {
				t.Errorf("%s %s: url %q sha256 %q", cli, arch, p.URL, p.SHA256)
			}
		}
	}
	// A bump records the fixtures again: the parsers are tested against what the pinned
	// versions print. (Grok Build's come with a real subscription test; see the PR.)
	for _, cli := range []string{"claude", "codex"} {
		name := filepath.Join("..", "..", "internal", "cliworker", "testdata", cli+"-"+manifest.CLIs[cli].Version+"-turn.jsonl")
		if _, err := os.Stat(name); err != nil {
			t.Errorf("no fixture recorded from the pinned %s: %v", cli, err)
		}
	}

	dockerfile := readRepoFile(t, "Dockerfile")
	for _, want := range []string{" AS cli-worker\n", "sh /src/cli-workers.sh install \"$TARGETARCH\"", "-perm /6000", "/usr/bin/tini"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the Dockerfile doesn't have %q", want)
		}
	}
	// The agent browser's programs are Debian's (Chromium, not Google Chrome: docs/agent-browser.md),
	// and the X socket directory is root's and sticky, so no user can take it over.
	for _, want := range []string{"chromium fonts-liberation x11vnc xvfb", "chmod 1777 /tmp/.X11-unix"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the Dockerfile doesn't have %q", want)
		}
	}
	if strings.Contains(dockerfile, "google-chrome") {
		t.Error("the image installs Google Chrome, whose terms don't allow shipping it")
	}
	// The server stays the default target: the last stage.
	stages := regexp.MustCompile(`(?m)^FROM .*$`).FindAllString(dockerfile, -1)
	if last := stages[len(stages)-1]; !strings.Contains(last, "distroless") {
		t.Errorf("the last stage is %q, not the server's", last)
	}

	compose := readRepoFile(t, "compose.yaml")
	i := strings.Index(compose, "\n  cli-worker:\n")
	if i < 0 {
		t.Fatal("no cli-worker service")
	}
	service := compose[i:]
	if j := strings.Index(service[1:], "\n  caddy:"); j > 0 {
		service = service[:j]
	}
	for _, want := range []string{"cap_drop: [ALL]", "cap_add: [SETUID, SETGID, CHOWN, KILL]", `security_opt: ["no-new-privileges:true"]`, "target: cli-worker", "65532:65532"} {
		if !strings.Contains(service, want) {
			t.Errorf("the cli-worker service doesn't have %q", want)
		}
	}
	// Nothing that would loosen the container for Chromium's sandbox (it runs without one,
	// as the user: docs/agent-browser.md).
	for _, never := range []string{"privileged", "/var/run/docker.sock", "- data:", "SYS_ADMIN", "seccomp", "apparmor"} {
		if strings.Contains(service, never) {
			t.Errorf("the cli-worker service has %q", never)
		}
	}
}

// The CI workflows: GitHub's, and Forgejo's, which runs the same jobs.
var ciFiles = []string{".github/workflows/ci.yml", ".forgejo/workflows/ci.yml"}

// The CI job that tests backups downloads the restic the backup image has, and the
// backup host runs the same restic for pruning: one version everywhere.
func TestBackupToolVersionsAgree(t *testing.T) {
	image := regexp.MustCompile(`(?m)^FROM restic/restic:(\S+)@sha256:\S+ AS backup$`).FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if image == nil {
		t.Fatal("no restic/restic stage named backup in the Dockerfile")
	}
	version := image[1]
	for _, name := range ciFiles {
		if !strings.Contains(readRepoFile(t, name), "releases/download/v"+version+"/restic_"+version+"_linux_") {
			t.Errorf("%s doesn't download restic %s, the backup image's version", name, version)
		}
	}
	if !strings.Contains(readRepoFile(t, "deploy/backup-host/compose.yaml"), "image: restic/restic:"+version+"@sha256:") {
		t.Errorf("the backup host's prune job doesn't run restic %s, the backup image's version", version)
	}
}

// deploy/INSTALL.md is run as is by an agent on a server open to the internet: it puts the
// CLI workers in container mode, the provider accounts' egress on public addresses, sets
// up backups the server can't delete and proves a restore, and never writes a development
// switch.
func TestInstallRunbookStaysSafe(t *testing.T) {
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	for _, want := range []string{"TASKTO_CLI_WORKERS=container", "TASKTO_PROVIDER_EGRESS_PERSONAL: public", "TASKTO_PROVIDER_EGRESS_WORKSPACE: public"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't set %s", want)
		}
	}
	// The owner's first password is one the install made, which root on the VM could read: the
	// runbook has them replace it.
	if !strings.Contains(runbook, "in **Settings → Password**, change it") {
		t.Error("deploy/INSTALL.md doesn't tell the owner to change the generated password")
	}
	// Backups are a required step: the nightly schedule, checked append-only from the VM,
	// and a restore drill into a scratch volume and a server cut off from the network. The
	// server's side never removes a snapshot, and the repository's secrets are never shown.
	for _, want := range []string{"## Step 15: backups", "## Step 16: the restore drill", "profiles=$profiles,backup-schedule",
		"backup-schedule -now", "-X DELETE", "dump latest /taskto.tar", "--network none", "restore -"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md's backups don't have %q", want)
		}
	}
	for _, never := range []string{"backup forget", "restic forget", "forget --", "Backups (optional)", `cat "$TASKTO_BACKUP_DIR`, "cat backup/", "cat /srv/taskto/backup/"} {
		if strings.Contains(runbook, never) {
			t.Errorf("deploy/INSTALL.md has %q", never)
		}
	}
	for _, never := range []string{"TASKTO_INSECURE_COOKIES=", "TASKTO_INSECURE_COOKIES:", "TASKTO_UNSAFE_PRIVILEGED_AGENTS=", "TASKTO_UNSAFE_PRIVILEGED_AGENTS:", "TASKTO_CLI_WORKERS=local", "-unsafe-privileged-agents", "--insecure"} {
		if strings.Contains(runbook, never) {
			t.Errorf("deploy/INSTALL.md has %q", never)
		}
	}
}

// The CLI worker's network reaches the internet and the per-turn proxy, nothing else
// (docs/cli-workers.md#the-workers-network). deploy/worker-network-test.sh proves it on a
// real Docker host, in CI; this keeps the files it proves from drifting.
func TestCLIWorkerNetwork(t *testing.T) {
	compose := readRepoFile(t, "compose.yaml")
	i, j := strings.Index(compose, "\n  cli-worker:\n"), strings.Index(compose, "\n  caddy:\n")
	if i < 0 || j < i {
		t.Fatal("no cli-worker service before the caddy one")
	}
	worker := compose[i:j]
	for _, want := range []string{"networks: [worker]", "-proxy-socket\n      - /run/taskto-cli/proxy/proxy.sock", "dns: ["} {
		if !strings.Contains(worker, want) {
			t.Errorf("the cli-worker service doesn't have %q", want)
		}
	}
	// No other service on the worker's network, and the server's proxy on the socket only.
	if n := strings.Count(compose, "[worker]") + strings.Count(compose, "- worker\n"); n != 1 {
		t.Errorf("%d services on the worker's network, not just the worker", n)
	}
	for _, want := range []string{
		"TASKTO_CLI_PROXY_LISTEN: unix:/run/taskto-cli/proxy/proxy.sock",
		"TASKTO_CLI_PROXY_URL: http://127.0.0.1:8081",
		"\n  worker:\n    enable_ipv6: false\n    driver_opts:\n      com.docker.network.bridge.name: taskto-worker\n",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml doesn't have %q", want)
		}
	}
	if strings.Contains(compose, "0.0.0.0:8081") || strings.Contains(compose, "taskto:8081") {
		t.Error("the proxy listens on the Compose network")
	}

	if !strings.Contains(readRepoFile(t, "deploy/worker-firewall.sh"), "\nbridge=taskto-worker\n") {
		t.Error("deploy/worker-firewall.sh isn't for compose.yaml's bridge, taskto-worker")
	}
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	for _, want := range []string{`git show "${TASKTO_VERSION}:deploy/worker-firewall.sh"`, "Before=docker.service", "/usr/local/sbin/taskto-worker-firewall check", "the worker reaches the server"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't have %q", want)
		}
	}
	for _, name := range ciFiles {
		ci := readRepoFile(t, name)
		for _, want := range []string{"sh deploy/worker-network-test.sh", "worker-network-test: OK"} {
			if !strings.Contains(ci, want) {
				t.Errorf("%s doesn't run the worker's network test (%q)", name, want)
			}
		}
	}
}
