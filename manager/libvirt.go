package main

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type VMState struct {
	Exists bool
	Active bool
	Name   string
}

type VMBackend interface {
	Validate(context.Context) error
	Start(context.Context, RunnerRecord, string) error
	State(context.Context, RunnerRecord) (VMState, error)
	Cleanup(context.Context, RunnerRecord) error
}

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return output, fmt.Errorf("%s: %w", name, err)
		}
		return output, fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	return output, nil
}

type LibvirtBackend struct {
	cfg     Config
	logger  *slog.Logger
	command commandRunner
}

func newLibvirtBackend(cfg Config, logger *slog.Logger) *LibvirtBackend {
	return &LibvirtBackend{cfg: cfg, logger: logger, command: execCommandRunner{}}
}

func (l *LibvirtBackend) Validate(ctx context.Context) error {
	if _, err := l.virsh(ctx, "pool-info", l.cfg.StoragePool); err != nil {
		return fmt.Errorf("storage pool %q: %w", l.cfg.StoragePool, err)
	}
	if _, err := l.virsh(ctx, "net-info", l.cfg.Network); err != nil {
		return fmt.Errorf("network %q: %w", l.cfg.Network, err)
	}
	if _, err := l.virsh(ctx, "vol-info", "--pool", l.cfg.StoragePool, l.cfg.GoldenVolume); err != nil {
		return fmt.Errorf("golden volume %q: %w", l.cfg.GoldenVolume, err)
	}
	return nil
}

func (l *LibvirtBackend) Start(ctx context.Context, record RunnerRecord, encodedJIT string) (retErr error) {
	if err := l.ensureRecordNames(record); err != nil {
		return err
	}
	capacity, err := l.goldenCapacity(ctx)
	if err != nil {
		return err
	}

	createdDisk := false
	createdSeed := false
	createdDomain := false
	defer func() {
		if retErr == nil {
			return
		}
		if createdDomain || createdSeed || createdDisk {
			cleanupCtx := context.WithoutCancel(ctx)
			if cleanupErr := l.Cleanup(cleanupCtx, record); cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("clean partial VM: %w", cleanupErr))
			}
		}
	}()

	if _, err := l.virsh(
		ctx,
		"vol-create-as",
		"--pool", l.cfg.StoragePool,
		"--name", record.DiskVolume,
		"--capacity", strconv.FormatUint(capacity, 10),
		"--format", "qcow2",
		"--backing-vol", l.cfg.GoldenVolume,
		"--backing-vol-format", "qcow2",
	); err != nil {
		return fmt.Errorf("create overlay volume: %w", err)
	}
	createdDisk = true

	tempDir, err := secureTempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	userData := buildCloudInitUserData(encodedJIT, l.cfg.GuestUser, l.cfg.GuestHTTPProxy)
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", record.Name, record.Name)
	userDataPath := filepath.Join(tempDir, "user-data")
	metaDataPath := filepath.Join(tempDir, "meta-data")
	seedPath := filepath.Join(tempDir, "seed.iso")
	if err := os.WriteFile(userDataPath, []byte(userData), 0o600); err != nil {
		return fmt.Errorf("write cloud-init user data: %w", err)
	}
	if err := os.WriteFile(metaDataPath, []byte(metaData), 0o600); err != nil {
		return fmt.Errorf("write cloud-init metadata: %w", err)
	}
	if _, err := l.command.Run(ctx, "cloud-localds", seedPath, userDataPath, metaDataPath); err != nil {
		return fmt.Errorf("create cloud-init seed: %w", err)
	}
	seedInfo, err := os.Stat(seedPath)
	if err != nil {
		return fmt.Errorf("inspect cloud-init seed: %w", err)
	}
	if _, err := l.virsh(
		ctx,
		"vol-create-as",
		"--pool", l.cfg.StoragePool,
		"--name", record.SeedVolume,
		"--capacity", strconv.FormatInt(seedInfo.Size(), 10),
		"--format", "raw",
	); err != nil {
		return fmt.Errorf("create seed volume: %w", err)
	}
	createdSeed = true
	if _, err := l.virsh(ctx, "vol-upload", "--pool", l.cfg.StoragePool, record.SeedVolume, seedPath); err != nil {
		return fmt.Errorf("upload seed volume: %w", err)
	}

	args := []string{
		"--connect", l.cfg.LibvirtURI,
		"--name", record.Domain,
		"--memory", strconv.Itoa(l.cfg.MemoryMiB),
		"--vcpus", strconv.Itoa(l.cfg.CPUs),
		"--cpu", "host-passthrough",
		"--machine", "q35",
		"--os-variant", "ubuntu24.04",
		"--import",
		"--disk", fmt.Sprintf("vol=%s/%s,device=disk,bus=virtio,cache=none,discard=unmap", l.cfg.StoragePool, record.DiskVolume),
		"--disk", fmt.Sprintf("vol=%s/%s,device=cdrom,bus=sata,readonly=on", l.cfg.StoragePool, record.SeedVolume),
		"--network", fmt.Sprintf("network=%s,model=virtio", l.cfg.Network),
		"--graphics", "spice,listen=none",
		"--video", "virtio",
		"--channel", "spicevmc",
		"--boot", "uefi",
		"--rng", "/dev/urandom",
		"--noautoconsole",
	}
	if _, err := l.command.Run(ctx, "virt-install", args...); err != nil {
		return fmt.Errorf("define and start domain: %w", err)
	}
	createdDomain = true

	l.logger.Info("runner VM started", "runner", record.Name, "domain", record.Domain)
	return nil
}

func (l *LibvirtBackend) State(ctx context.Context, record RunnerRecord) (VMState, error) {
	if err := l.ensureRecordNames(record); err != nil {
		return VMState{}, err
	}
	exists, err := l.domainExists(ctx, record.Domain)
	if err != nil || !exists {
		return VMState{Exists: exists}, err
	}
	output, err := l.virsh(ctx, "domstate", record.Domain)
	if err != nil {
		return VMState{}, fmt.Errorf("read domain state: %w", err)
	}
	name := strings.ToLower(strings.TrimSpace(string(output)))
	active := name != "shut off" && name != "crashed"
	return VMState{Exists: true, Active: active, Name: name}, nil
}

func (l *LibvirtBackend) Cleanup(ctx context.Context, record RunnerRecord) error {
	if err := l.ensureRecordNames(record); err != nil {
		return err
	}
	var cleanupErrors []error

	exists, err := l.domainExists(ctx, record.Domain)
	if err != nil {
		cleanupErrors = append(cleanupErrors, err)
	} else if exists {
		state, stateErr := l.State(ctx, record)
		if stateErr != nil {
			cleanupErrors = append(cleanupErrors, stateErr)
		} else if state.Active {
			if _, destroyErr := l.virsh(ctx, "destroy", record.Domain); destroyErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("destroy domain: %w", destroyErr))
			}
		}
		if _, undefineErr := l.virsh(ctx, "undefine", record.Domain, "--nvram"); undefineErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("undefine domain: %w", undefineErr))
		}
	}

	for _, volume := range []string{record.SeedVolume, record.DiskVolume} {
		exists, volumeErr := l.volumeExists(ctx, volume)
		if volumeErr != nil {
			cleanupErrors = append(cleanupErrors, volumeErr)
			continue
		}
		if !exists {
			continue
		}
		if _, deleteErr := l.virsh(ctx, "vol-delete", "--pool", l.cfg.StoragePool, volume); deleteErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete volume %q: %w", volume, deleteErr))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (l *LibvirtBackend) virsh(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := append([]string{"--connect", l.cfg.LibvirtURI}, args...)
	return l.command.Run(ctx, "virsh", commandArgs...)
}

func (l *LibvirtBackend) goldenCapacity(ctx context.Context) (uint64, error) {
	output, err := l.virsh(ctx, "vol-dumpxml", "--pool", l.cfg.StoragePool, l.cfg.GoldenVolume)
	if err != nil {
		return 0, fmt.Errorf("read golden volume XML: %w", err)
	}
	var volume struct {
		Capacity uint64 `xml:"capacity"`
	}
	if err := xml.Unmarshal(output, &volume); err != nil {
		return 0, fmt.Errorf("decode golden volume XML: %w", err)
	}
	if volume.Capacity == 0 {
		return 0, errors.New("golden volume reports zero capacity")
	}
	return volume.Capacity, nil
}

func (l *LibvirtBackend) domainExists(ctx context.Context, name string) (bool, error) {
	_, err := l.virsh(ctx, "domuuid", name)
	if err == nil {
		return true, nil
	}
	if resourceMissing(err) {
		return false, nil
	}
	return false, fmt.Errorf("check domain %q: %w", name, err)
}

func (l *LibvirtBackend) volumeExists(ctx context.Context, name string) (bool, error) {
	_, err := l.virsh(ctx, "vol-key", "--pool", l.cfg.StoragePool, name)
	if err == nil {
		return true, nil
	}
	if resourceMissing(err) {
		return false, nil
	}
	return false, fmt.Errorf("check volume %q: %w", name, err)
}

func resourceMissing(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "failed to get domain") ||
		strings.Contains(message, "failed to get vol") ||
		strings.Contains(message, "no storage vol")
}

func (l *LibvirtBackend) ensureRecordNames(record RunnerRecord) error {
	wantPrefix := l.cfg.DomainPrefix + "-"
	for kind, value := range map[string]string{
		"runner":      record.Name,
		"domain":      record.Domain,
		"disk volume": record.DiskVolume,
		"seed volume": record.SeedVolume,
	} {
		if !strings.HasPrefix(value, wantPrefix) || !safeNamePattern.MatchString(value) {
			return fmt.Errorf("refusing to manage unsafe %s name %q", kind, value)
		}
	}
	return nil
}

func secureTempDir() (string, error) {
	for _, base := range []string{"/dev/shm", ""} {
		dir, err := os.MkdirTemp(base, "kvm-runner-seed-")
		if err != nil {
			continue
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			os.RemoveAll(dir)
			continue
		}
		return dir, nil
	}
	return "", errors.New("create secure temporary directory")
}

// guestNoProxy keeps the host caches (Verdaccio, the registry mirror, NFS on 192.168.122.1) and private ranges off the proxy.
const guestNoProxy = "localhost,127.0.0.1,::1,192.168.0.0/16,10.0.0.0/8,172.16.0.0/12"

func buildCloudInitUserData(encodedJIT, user, proxy string) string {
	wrapper := `#!/usr/bin/env bash
set -uo pipefail

finish() {
  rc=$?
  trap - EXIT HUP INT TERM
  unset jit
  rm -f /run/actions-runner-jit
  sync
  systemctl poweroff --no-block || true
  exit "$rc"
}
trap finish EXIT HUP INT TERM

jit="$(</run/actions-runner-jit)"
rm -f /run/actions-runner-jit
install -o @USER@ -g @USER@ -m 0750 -d /home/@USER@/actions-runner-logs
runuser -u @USER@ -- env \
  HOME=/home/@USER@ \
  USER=@USER@ \
  LOGNAME=@USER@ \
  PATH=/home/@USER@/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
  /opt/actions-runner/run.sh --jitconfig "$jit" \
  2>&1 | tee -a /home/@USER@/actions-runner-logs/runner.log
exit "${PIPESTATUS[0]}"
`
	wrapper = strings.ReplaceAll(wrapper, "@USER@", user)
	service := `[Unit]
Description=One-shot ephemeral GitHub Actions runner
Wants=network-online.target docker.service
After=network-online.target docker.service
ConditionPathExists=/run/actions-runner-jit

[Service]
Type=simple
EnvironmentFile=-/etc/default/kvm-actions-runner
ExecStart=/usr/local/sbin/kvm-actions-runner-once
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`
	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	// The proxy is injected at boot, never baked into the image: the runner and job steps read the EnvironmentFile
	// (runuser keeps the environment), dockerd reads a drop-in; dockerd may start before cloud-init, hence try-restart.
	proxyFiles, proxyCommands := "", ""
	if proxy != "" {
		runnerEnv := ""
		dockerEnv := "[Service]\n"
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			runnerEnv += name + "=" + proxy + "\n"
			dockerEnv += fmt.Sprintf("Environment=%q\n", name+"="+proxy)
		}
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			runnerEnv += name + "=" + guestNoProxy + "\n"
			dockerEnv += fmt.Sprintf("Environment=%q\n", name+"="+guestNoProxy)
		}
		proxyFiles = fmt.Sprintf(`  - path: /etc/default/kvm-actions-runner
    owner: root:root
    permissions: '0644'
    encoding: b64
    content: %s
  - path: /etc/systemd/system/docker.service.d/http-proxy.conf
    owner: root:root
    permissions: '0644'
    encoding: b64
    content: %s
`, encode(runnerEnv), encode(dockerEnv))
		proxyCommands = "  - [ systemctl, try-restart, docker.service ]\n"
	}
	return fmt.Sprintf(`#cloud-config
write_files:
  - path: /run/actions-runner-jit
    owner: root:root
    permissions: '0600'
    encoding: b64
    content: %s
  - path: /usr/local/sbin/kvm-actions-runner-once
    owner: root:root
    permissions: '0750'
    encoding: b64
    content: %s
  - path: /etc/systemd/system/github-actions-runner.service
    owner: root:root
    permissions: '0644'
    encoding: b64
    content: %s
%sruncmd:
  - [ systemctl, daemon-reload ]
%s  - [ systemctl, enable, github-actions-runner.service ]
  - [ systemctl, start, --no-block, github-actions-runner.service ]
`, encode(encodedJIT), encode(wrapper), encode(service), proxyFiles, proxyCommands)
}
