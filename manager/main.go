package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

const managerVersion = "0.1.0"

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	logger := newLogger(cfg.LogLevel)
	if err := run(cfg, logger); err != nil {
		logger.Error("manager stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend := newLibvirtBackend(cfg, logger.WithGroup("libvirt"))
	if err := backend.Validate(ctx); err != nil {
		return fmt.Errorf("validate libvirt: %w", err)
	}
	if cfg.ValidateOnly {
		logger.Info("local configuration is valid", "golden_volume", cfg.GoldenVolume, "storage_pool", cfg.StoragePool)
		return nil
	}

	lock, err := acquireProcessLock(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	store, err := openStateStore(cfg.StateDir)
	if err != nil {
		return err
	}

	client, err := newScaleSetClient(cfg)
	if err != nil {
		return err
	}
	runnerGroupID, err := resolveRunnerGroup(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return err
	}
	scaleSet, created, err := ensureScaleSet(ctx, client, cfg, runnerGroupID)
	if err != nil {
		return err
	}
	client.SetSystemInfo(systemInfo(scaleSet.ID))
	if err := store.Bind(cfg.RegistrationURL, cfg.ScaleSetName, scaleSet.ID); err != nil {
		return fmt.Errorf("bind persistent state: %w", err)
	}
	logger.Info("runner scale set ready", "name", scaleSet.Name, "id", scaleSet.ID, "created", created, "url", cfg.RegistrationURL)

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "kvm-gha-runner"
	}
	owner := hostname + "-" + cfg.ScaleSetName
	sessionClient, err := client.MessageSessionClient(ctx, scaleSet.ID, owner)
	if err != nil {
		return fmt.Errorf("create GitHub message session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sessionClient.Close(closeCtx); err != nil {
			logger.Warn("failed to close GitHub message session", "error", err)
		}
	}()

	messageListener, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: cfg.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("initialize official scale set listener: %w", err)
	}
	scaler := &Scaler{
		logger:       logger.WithGroup("scaler"),
		store:        store,
		backend:      backend,
		client:       client,
		scaleSetID:   scaleSet.ID,
		minRunners:   cfg.MinRunners,
		maxRunners:   cfg.MaxRunners,
		domainPrefix: cfg.DomainPrefix,
		retainFailed: cfg.RetainFailedVMs,
		maxFailed:    cfg.MaxRetainedFailed,
	}

	logger.Info("listening for assigned GitHub Actions jobs", "min_runners", cfg.MinRunners, "max_runners", cfg.MaxRunners)
	if err := messageListener.Run(ctx, scaler); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("official scale set listener: %w", err)
	}
	logger.Info("manager shutdown requested; running VMs are left intact for restart recovery")
	return nil
}

func newScaleSetClient(cfg Config) (*scaleset.Client, error) {
	info := systemInfo(0)
	if cfg.TokenFile != "" {
		tokenBytes, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read GitHub token: %w", err)
		}
		token := strings.TrimSpace(string(tokenBytes))
		if token == "" {
			return nil, errors.New("GitHub token file is empty")
		}
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     cfg.RegistrationURL,
			PersonalAccessToken: token,
			SystemInfo:          info,
		})
	}

	privateKey, err := os.ReadFile(cfg.GitHubAppPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.RegistrationURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       cfg.GitHubAppClientID,
			InstallationID: cfg.GitHubAppInstallID,
			PrivateKey:     string(privateKey),
		},
		SystemInfo: info,
	})
}

func resolveRunnerGroup(ctx context.Context, client *scaleset.Client, name string) (int, error) {
	if name == scaleset.DefaultRunnerGroup {
		return 1, nil
	}
	group, err := client.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("get runner group %q: %w", name, err)
	}
	return group.ID, nil
}

func ensureScaleSet(ctx context.Context, client *scaleset.Client, cfg Config, groupID int) (*scaleset.RunnerScaleSet, bool, error) {
	wantedLabels := make([]scaleset.Label, 0, len(cfg.Labels))
	for _, label := range cfg.Labels {
		wantedLabels = append(wantedLabels, scaleset.Label{Name: label})
	}

	existing, err := client.GetRunnerScaleSet(ctx, groupID, cfg.ScaleSetName)
	if err != nil {
		return nil, false, fmt.Errorf("look up runner scale set: %w", err)
	}
	if existing == nil {
		created, err := client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          cfg.ScaleSetName,
			RunnerGroupID: groupID,
			Labels:        wantedLabels,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return nil, false, fmt.Errorf("create runner scale set: %w", err)
		}
		return created, true, nil
	}

	if sameLabelNames(existing.Labels, wantedLabels) && existing.RunnerSetting.DisableUpdate {
		return existing, false, nil
	}
	existing.Labels = wantedLabels
	existing.RunnerSetting.DisableUpdate = true
	updated, err := client.UpdateRunnerScaleSet(ctx, existing.ID, existing)
	if err != nil {
		return nil, false, fmt.Errorf("update runner scale set: %w", err)
	}
	return updated, false, nil
}

func sameLabelNames(left, right []scaleset.Label) bool {
	leftNames := make([]string, 0, len(left))
	rightNames := make([]string, 0, len(right))
	for _, label := range left {
		leftNames = append(leftNames, label.Name)
	}
	for _, label := range right {
		rightNames = append(rightNames, label.Name)
	}
	slices.Sort(leftNames)
	slices.Sort(rightNames)
	return slices.Equal(leftNames, rightNames)
}

func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "kvm-gha-runner",
		Subsystem:  "listener",
		Version:    managerVersion,
		CommitSHA:  "local",
		ScaleSetID: scaleSetID,
	}
}

func newLogger(levelName string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelName) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func acquireProcessLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory for lock: %w", err)
	}
	path := filepath.Join(stateDir, "manager.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another manager already holds %q: %w", path, err)
	}
	return file, nil
}
