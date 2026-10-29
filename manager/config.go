package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	RegistrationURL     string
	ScaleSetName        string
	RunnerGroup         string
	Labels              []string
	MinRunners          int
	MaxRunners          int
	CPUs                int
	MemoryMiB           int
	DiskGiB             int
	LibvirtURI          string
	StoragePool         string
	Network             string
	GoldenVolume        string
	GuestHTTPProxy      string
	GuestUser           string
	DomainPrefix        string
	StateDir            string
	TokenFile           string
	GitHubAppClientID   string
	GitHubAppInstallID  int64
	GitHubAppPrivateKey string
	RetainFailedVMs     bool
	MaxRetainedFailed   int
	LogLevel            string
	ValidateOnly        bool
}

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func parseConfig(args []string) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home: %w", err)
	}

	var cfg Config
	var labels string
	flags := flag.NewFlagSet("kvm-gha-runner", flag.ContinueOnError)
	flags.StringVar(&cfg.RegistrationURL, "url", "", "GitHub repository or organization URL")
	flags.StringVar(&cfg.ScaleSetName, "name", "kvm-runner-set", "runner scale set name")
	flags.StringVar(&cfg.RunnerGroup, "runner-group", "default", "runner group name")
	flags.StringVar(&labels, "labels", "", "comma-separated runner labels; defaults to the scale set name")
	flags.IntVar(&cfg.MinRunners, "min-runners", 0, "minimum warm runners")
	flags.IntVar(&cfg.MaxRunners, "max-runners", 2, "maximum concurrent runners")
	flags.IntVar(&cfg.CPUs, "cpus", 4, "vCPUs per runner VM")
	flags.IntVar(&cfg.MemoryMiB, "memory-mib", 8192, "memory per runner VM in MiB")
	flags.IntVar(&cfg.DiskGiB, "disk-gib", 0, "disk per runner VM in GiB; never smaller than the golden image")
	flags.StringVar(&cfg.LibvirtURI, "libvirt-uri", "qemu:///system", "libvirt connection URI")
	flags.StringVar(&cfg.StoragePool, "storage-pool", "default", "libvirt storage pool")
	flags.StringVar(&cfg.Network, "network", "default", "libvirt network")
	flags.StringVar(&cfg.GoldenVolume, "golden-volume", "kvm-gha-slim-v1.qcow2", "read-only golden QCOW2 volume")
	flags.StringVar(&cfg.GuestHTTPProxy, "guest-http-proxy", "", "HTTP proxy URL injected into runner VMs (runner, job steps and dockerd); empty disables")
	flags.StringVar(&cfg.GuestUser, "guest-user", "runner", "user inside the golden image that runs the Actions runner")
	flags.StringVar(&cfg.DomainPrefix, "domain-prefix", "kvm-gha", "prefix for ephemeral domains and volumes")
	flags.StringVar(&cfg.StateDir, "state-dir", filepath.Join(home, ".local", "state", "kvm-gha-runner"), "persistent local state directory")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "file containing a GitHub token; never pass a token directly")
	flags.StringVar(&cfg.GitHubAppClientID, "app-client-id", "", "GitHub App client ID or App ID")
	flags.Int64Var(&cfg.GitHubAppInstallID, "app-installation-id", 0, "GitHub App installation ID")
	flags.StringVar(&cfg.GitHubAppPrivateKey, "app-private-key-file", "", "GitHub App private key PEM file")
	flags.BoolVar(&cfg.RetainFailedVMs, "retain-failed-vms", true, "retain unexpectedly stopped VMs for diagnostics")
	flags.IntVar(&cfg.MaxRetainedFailed, "max-retained-failed", 2, "maximum failed VMs retained for diagnostics")
	flags.StringVar(&cfg.LogLevel, "log-level", "info", "debug, info, warn, or error")
	flags.BoolVar(&cfg.ValidateOnly, "validate-only", false, "validate local configuration without calling GitHub")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	if strings.TrimSpace(labels) == "" {
		cfg.Labels = []string{cfg.ScaleSetName}
	} else {
		for _, label := range strings.Split(labels, ",") {
			cfg.Labels = append(cfg.Labels, strings.TrimSpace(label))
		}
	}

	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	parsedURL, err := url.ParseRequestURI(c.RegistrationURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return fmt.Errorf("--url must be a full HTTPS GitHub repository or organization URL")
	}
	if !safeNamePattern.MatchString(c.ScaleSetName) {
		return fmt.Errorf("invalid scale set name %q", c.ScaleSetName)
	}
	if !safeNamePattern.MatchString(c.RunnerGroup) {
		return fmt.Errorf("invalid runner group %q", c.RunnerGroup)
	}
	if len(c.Labels) == 0 {
		return errors.New("at least one runner label is required")
	}
	for _, label := range c.Labels {
		if !safeNamePattern.MatchString(label) {
			return fmt.Errorf("invalid runner label %q", label)
		}
	}
	if c.MinRunners < 0 || c.MaxRunners < 0 || c.MaxRunners < c.MinRunners {
		return errors.New("runner counts must satisfy 0 <= min-runners <= max-runners")
	}
	if c.CPUs < 1 || c.CPUs > 64 {
		return errors.New("--cpus must be between 1 and 64")
	}
	if c.MemoryMiB < 1024 {
		return errors.New("--memory-mib must be at least 1024")
	}
	if c.DiskGiB < 0 || c.DiskGiB > 4096 {
		return errors.New("--disk-gib must be between 0 and 4096")
	}
	if c.MaxRetainedFailed < 0 {
		return errors.New("--max-retained-failed must not be negative")
	}
	for option, value := range map[string]string{
		"storage-pool":  c.StoragePool,
		"network":       c.Network,
		"golden-volume": c.GoldenVolume,
		"domain-prefix": c.DomainPrefix,
		"guest-user":    c.GuestUser,
	} {
		if !safeNamePattern.MatchString(value) {
			return fmt.Errorf("invalid --%s value %q", option, value)
		}
	}
	if c.GuestHTTPProxy != "" {
		proxyURL, err := url.Parse(c.GuestHTTPProxy)
		if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Host == "" || proxyURL.User != nil {
			return errors.New("--guest-http-proxy must be an http(s)://host:port URL without credentials")
		}
	}
	if len(c.DomainPrefix) > 40 {
		return errors.New("--domain-prefix must not exceed 40 characters")
	}

	hasToken := c.TokenFile != ""
	hasAnyApp := c.GitHubAppClientID != "" || c.GitHubAppInstallID != 0 || c.GitHubAppPrivateKey != ""
	hasCompleteApp := c.GitHubAppClientID != "" && c.GitHubAppInstallID != 0 && c.GitHubAppPrivateKey != ""
	if hasToken == hasAnyApp {
		return errors.New("configure exactly one authentication method: --token-file or all GitHub App options")
	}
	if hasAnyApp && !hasCompleteApp {
		return errors.New("GitHub App authentication requires client ID, installation ID, and private key file")
	}
	for _, path := range []string{c.TokenFile, c.GitHubAppPrivateKey} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("credential file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("credential path %q is not a regular file", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("credential file %q must not be accessible by group or others", path)
		}
	}
	for _, command := range []string{"cloud-localds", "virsh", "virt-install"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("required command %q is unavailable: %w", command, err)
		}
	}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return errors.New("--state-dir must be an absolute path")
	}
	return nil
}
