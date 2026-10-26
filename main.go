package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultDeviceName = "OBS_INGEST"

//go:embed assets/no_signal.avif
var bundledFallback []byte

type config struct {
	display        string
	titlePattern   string
	classPattern   string
	processPattern string
	output         string
	frameRate      int
	pollInterval   time.Duration
	videoFilter    string
	fallbackImage  string
	ffmpegPath     string
	xdotoolPath    string
	dryRun         bool
}

type matcher struct {
	title   *regexp.Regexp
	class   *regexp.Regexp
	process *regexp.Regexp
}

type window struct {
	id      uint64
	title   string
	class   string
	pid     int
	process string
}

type windowIdentity struct {
	id  uint64
	pid int
}

type capture struct {
	windowID uint64
	cmd      *exec.Cmd
	done     chan error
}

type checkResult struct {
	name     string
	detail   string
	required bool
	ok       bool
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		if err := setup(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		cfg := parseFlags(os.Args[2:])
		if err := resolveFallbackImage(&cfg); err != nil {
			log.Fatal(err)
		}
		match, err := newMatcher(cfg)
		if err != nil {
			log.Fatal(err)
		}
		if !printChecks(runChecks(context.Background(), cfg, match), os.Stdout) {
			os.Exit(1)
		}
		return
	}
	watch := len(os.Args) > 1 && os.Args[1] == "watch"
	args := os.Args[1:]
	if watch {
		args = args[1:]
	}

	cfg := parseFlags(args)
	cfg.dryRun = watch
	if err := resolveFallbackImage(&cfg); err != nil {
		log.Fatal(err)
	}
	match, err := newMatcher(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if !cfg.dryRun && cfg.output == "" {
		cfg.output, err = findVideoDeviceByName(defaultDeviceName)
		if err != nil {
			log.Fatal(err)
		}
		if cfg.output == "" {
			log.Fatalf("virtual camera %q was not found; run %s setup first", defaultDeviceName, os.Args[0])
		}
	}
	if !cfg.dryRun && !printChecks(runChecks(context.Background(), cfg, match), os.Stdout) {
		log.Fatal("preflight failed; fix the required checks above or run doctor for details")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = run(ctx, cfg, match)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func runChecks(ctx context.Context, cfg config, match matcher) []checkResult {
	var results []checkResult
	checkTool := func(name, configured string) {
		path, err := exec.LookPath(configured)
		results = append(results, checkResult{
			name: name, required: true, ok: err == nil,
			detail: choose(err == nil, path, fmt.Sprintf("not found: %s", configured)),
		})
	}
	checkTool("ffmpeg", cfg.ffmpegPath)
	checkTool("xdotool", cfg.xdotoolPath)
	moduleLoaded := pathExists("/sys/module/v4l2loopback")
	results = append(results, checkResult{name: "v4l2loopback", required: true, ok: moduleLoaded,
		detail: choose(moduleLoaded, "kernel module loaded", "kernel module is not loaded")})

	device := cfg.output
	discoveredByName := device == ""
	if device == "" {
		device, _ = findVideoDeviceByName(defaultDeviceName)
	}
	deviceOK := device != "" && pathExists(device)
	ctl, ctlErr := exec.LookPath("v4l2loopback-ctl")
	ctlDetail := "not found in PATH; needed only to create a missing virtual camera"
	if ctlErr == nil {
		ctlDetail = ctl
	} else if deviceOK {
		ctlDetail = "not found in PATH; not needed because the virtual camera already exists"
	}
	results = append(results, checkResult{name: "v4l2loopback-ctl", required: false, ok: ctlErr == nil,
		detail: ctlDetail})
	detail := fmt.Sprintf("%s not found; run setup", defaultDeviceName)
	if deviceOK {
		if discoveredByName {
			detail = fmt.Sprintf("%s already exists at %s", defaultDeviceName, device)
		} else {
			detail = fmt.Sprintf("using explicit device %s", device)
		}
	}
	results = append(results, checkResult{name: "virtual camera", required: true, ok: deviceOK, detail: detail})
	if deviceOK {
		loopback, err := isLoopbackDevice(device)
		results = append(results, checkResult{name: "camera type", required: true, ok: err == nil && loopback,
			detail: choose(err == nil && loopback, "v4l2loopback", "device is not a v4l2loopback camera")})
		writableErr := syscall.Access(device, 2) // W_OK is 2 on Linux.
		writable := writableErr == nil
		results = append(results, checkResult{name: "device writable", required: true, ok: writable,
			detail: choose(writable, "write permission is present", "current user cannot write to the device")})
	}

	displayCmd := exec.CommandContext(ctx, cfg.xdotoolPath, "getmouselocation")
	displayCmd.Env = append(os.Environ(), "DISPLAY="+cfg.display)
	displayErr := displayCmd.Run()
	results = append(results, checkResult{name: "X11 display", required: true, ok: displayErr == nil,
		detail: choose(displayErr == nil, cfg.display, fmt.Sprintf("cannot connect to %s: %v", cfg.display, displayErr))})

	if cfg.fallbackImage != "" {
		_, err := os.Stat(cfg.fallbackImage)
		results = append(results, checkResult{name: "fallback image", required: false, ok: err == nil,
			detail: choose(err == nil, cfg.fallbackImage, errString(err))})
	}

	windows, err := discoverWindows(ctx, cfg)
	found := false
	if err == nil {
		_, found = selectWindow(windows, match, 0, 0, windowIdentity{})
	}
	results = append(results, checkResult{name: "matching window", required: false, ok: found,
		detail: choose(found, "currently available", "not currently available; capture will wait")})
	return results
}

func printChecks(results []checkResult, output *os.File) bool {
	allRequired := true
	for _, result := range results {
		status := "OK"
		if !result.ok {
			if result.required {
				status = "FAIL"
				allRequired = false
			} else {
				status = "INFO"
			}
		}
		fmt.Fprintf(output, "%-4s %-18s %s\n", status, result.name, result.detail)
	}
	return allRequired
}

func choose[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func parseFlags(args []string) config {
	var cfg config
	defaultDisplay := os.Getenv("DISPLAY")
	if defaultDisplay == "" {
		defaultDisplay = ":0"
	}

	flags := flag.NewFlagSet("window-capture-follow", flag.ExitOnError)
	flags.StringVar(&cfg.display, "display", defaultDisplay, "X11 display to inspect and capture")
	flags.StringVar(&cfg.titlePattern, "title", `(?i)^Brood War$`, "regular expression matched against the window title")
	flags.StringVar(&cfg.classPattern, "class", `(?i)^starcraft\.exe$`, "regular expression matched against the X11 window class")
	flags.StringVar(&cfg.processPattern, "process", "", "regular expression matched against the owning process command line")
	flags.StringVar(&cfg.output, "output", "", "V4L2 output device override (normally discovered by name)")
	flags.IntVar(&cfg.frameRate, "fps", 30, "capture frame rate")
	flags.DurationVar(&cfg.pollInterval, "poll", time.Second, "window discovery interval")
	flags.StringVar(&cfg.videoFilter, "video-filter", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,format=yuyv422", "advanced ffmpeg video filter override")
	flags.StringVar(&cfg.fallbackImage, "fallback-image", "bundled", "image shown while no matching window exists; empty disables it")
	flags.StringVar(&cfg.ffmpegPath, "ffmpeg", "ffmpeg", "ffmpeg executable")
	flags.StringVar(&cfg.xdotoolPath, "xdotool", "xdotool", "xdotool executable")
	_ = flags.Parse(args)

	if cfg.frameRate <= 0 {
		log.Fatal("--fps must be greater than zero")
	}
	if cfg.pollInterval <= 0 {
		log.Fatal("--poll must be greater than zero")
	}
	if cfg.titlePattern == "" && cfg.classPattern == "" && cfg.processPattern == "" {
		log.Fatal("at least one of --title, --class, or --process is required")
	}
	return cfg
}

func resolveFallbackImage(cfg *config) error {
	if cfg.fallbackImage != "bundled" {
		return nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("locate cache directory for bundled fallback: %w", err)
	}
	dir := filepath.Join(cacheDir, "window-capture-follow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create fallback cache directory: %w", err)
	}
	path := filepath.Join(dir, "no_signal.avif")
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, bundledFallback) {
		cfg.fallbackImage = path
		return nil
	}
	if err := os.WriteFile(path, bundledFallback, 0o644); err != nil {
		return fmt.Errorf("write bundled fallback image: %w", err)
	}
	cfg.fallbackImage = path
	return nil
}

func setup(args []string, input *os.File, output *os.File) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(output)
	device := flags.String("device", "", "specific video device override, for example /dev/video20")
	name := flags.String("name", defaultDeviceName, "virtual camera name")
	yes := flags.Bool("yes", false, "skip confirmation")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	existing, err := findVideoDeviceByName(*name)
	if err != nil {
		return err
	}
	if existing != "" {
		fmt.Fprintf(output, "%s already exists as %s. Nothing to do.\n", *name, existing)
		return nil
	}
	if *device != "" {
		if exists, err := isLoopbackDevice(*device); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%s already exists with a different name", *device)
		}
		if _, err := os.Stat(*device); err == nil {
			return fmt.Errorf("%s already exists but is not a v4l2loopback device", *device)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	ctl, err := findLoopbackCtl()
	if err != nil {
		return err
	}
	if *device == "" {
		fmt.Fprintf(output, "This will create a free virtual video device named %q using sudo.\n", *name)
	} else {
		fmt.Fprintf(output, "This will create %s named %q using sudo.\n", *device, *name)
	}
	fmt.Fprintln(output, "It will not modify or remove existing video devices.")
	if !*yes {
		fmt.Fprint(output, "Continue? [y/N] ")
		scanner := bufio.NewScanner(input)
		if !scanner.Scan() || !strings.EqualFold(strings.TrimSpace(scanner.Text()), "y") {
			fmt.Fprintln(output, "Cancelled.")
			return nil
		}
	}

	commandArgs := []string{"add", "--name", *name, "--exclusive-caps", "1"}
	if *device != "" {
		commandArgs = append(commandArgs, *device)
	}
	cmd := exec.Command("sudo", append([]string{ctl}, commandArgs...)...)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create virtual camera: %w", err)
	}
	created, err := findVideoDeviceByName(*name)
	if err != nil {
		return err
	}
	if created == "" {
		return errors.New("setup command completed but the virtual camera is not available")
	}
	fmt.Fprintf(output, "Created %s as %s. Select it as the video source in your streaming application.\n", *name, created)
	return nil
}

func findVideoDeviceByName(name string) (string, error) {
	paths, err := filepath.Glob("/sys/class/video4linux/video*/name")
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(data)) == name {
			return filepath.Join("/dev", filepath.Base(filepath.Dir(path))), nil
		}
	}
	return "", nil
}

func isLoopbackDevice(device string) (bool, error) {
	base := filepath.Base(device)
	if !strings.HasPrefix(base, "video") || strings.TrimPrefix(base, "video") == "" {
		return false, fmt.Errorf("invalid video device path %q", device)
	}
	path := filepath.Join("/sys/class/video4linux", base, "name")
	_, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// v4l2loopback exposes these controls for every dynamically managed device.
	for _, control := range []string{"buffers", "max_openers"} {
		if _, err := os.Stat(filepath.Join("/sys/class/video4linux", base, control)); err != nil {
			return false, nil
		}
	}
	return true, nil
}

func findLoopbackCtl() (string, error) {
	path, err := exec.LookPath("v4l2loopback-ctl")
	if err != nil {
		return "", errors.New("v4l2loopback-ctl was not found in PATH; install the v4l2loopback utilities first")
	}
	return path, nil
}

func newMatcher(cfg config) (matcher, error) {
	var match matcher
	patterns := []struct {
		name  string
		value string
		dest  **regexp.Regexp
	}{
		{"title", cfg.titlePattern, &match.title},
		{"class", cfg.classPattern, &match.class},
		{"process", cfg.processPattern, &match.process},
	}
	for _, pattern := range patterns {
		if pattern.value == "" {
			continue
		}
		re, err := regexp.Compile(pattern.value)
		if err != nil {
			return matcher{}, fmt.Errorf("invalid --%s expression: %w", pattern.name, err)
		}
		*pattern.dest = re
	}
	return match, nil
}

func (m matcher) matches(w window) bool {
	return (m.title == nil || m.title.MatchString(w.title)) &&
		(m.class == nil || m.class.MatchString(w.class)) &&
		(m.process == nil || m.process.MatchString(w.process))
}

func run(ctx context.Context, cfg config, match matcher) error {
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	var active *capture
	var selectedID uint64
	var selectedPID int
	var failed windowIdentity
	fallbackFailed := false
	defer func() { stopCapture(active) }()

	for {
		windows, err := discoverWindows(ctx, cfg)
		if err != nil {
			log.Printf("window discovery failed: %v", err)
		} else {
			if failed.id != 0 && !containsWindow(windows, failed) {
				failed = windowIdentity{}
			}
			selected, ok := selectWindow(windows, match, selectedID, selectedPID, failed)
			if !ok {
				if selectedID != 0 {
					log.Printf("matching window 0x%x disappeared; waiting for a replacement", selectedID)
				}
				selectedID = 0
				selectedPID = 0
				if !cfg.dryRun && (active == nil || active.windowID != 0) {
					stopCapture(active)
					active = nil
					if cfg.fallbackImage != "" && !fallbackFailed {
						active, err = startFallback(ctx, cfg)
						if err != nil {
							log.Printf("could not start fallback image: %v", err)
							fallbackFailed = true
						}
					}
				}
			} else if selected.id != selectedID || selected.pid != selectedPID {
				stopCapture(active)
				active = nil
				selectedID = selected.id
				selectedPID = selected.pid
				log.Printf("following window 0x%x title=%q class=%q pid=%d process=%q",
					selected.id, selected.title, selected.class, selected.pid, selected.process)
				if !cfg.dryRun {
					active, err = startCapture(ctx, cfg, selected)
					if err != nil {
						log.Printf("could not start capture: %v", err)
					}
				}
			}
		}

		var captureDone <-chan error
		if active != nil {
			captureDone = active.done
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-captureDone:
			if err != nil {
				log.Printf("ffmpeg stopped: %v", err)
			} else {
				log.Printf("ffmpeg stopped")
			}
			if active.windowID != 0 {
				failed = windowIdentity{id: selectedID, pid: selectedPID}
			} else {
				fallbackFailed = true
			}
			active = nil
			selectedID = 0
			selectedPID = 0
		case <-ticker.C:
		}
	}
}

func startFallback(ctx context.Context, cfg config) (*capture, error) {
	if _, err := os.Stat(cfg.fallbackImage); err != nil {
		return nil, err
	}
	args := []string{
		"-nostdin", "-loglevel", "warning",
		"-stream_loop", "-1", "-re",
		"-i", cfg.fallbackImage,
		"-vf", "fps=" + strconv.Itoa(cfg.frameRate) + "," + cfg.videoFilter,
		"-f", "v4l2", cfg.output,
	}
	cmd := exec.CommandContext(ctx, cfg.ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("showing fallback image %s", cfg.fallbackImage)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return &capture{cmd: cmd, done: done}, nil
}

func discoverWindows(ctx context.Context, cfg config) ([]window, error) {
	output, err := commandOutput(ctx, cfg, "search", "--onlyvisible", "--name", ".*")
	if err != nil {
		// xdotool exits with status 1 when no windows match.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	var windows []window
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		id, err := strconv.ParseUint(strings.TrimSpace(scanner.Text()), 10, 64)
		if err != nil {
			continue
		}
		windows = append(windows, inspectWindow(ctx, cfg, id))
	}
	return windows, scanner.Err()
}

func inspectWindow(ctx context.Context, cfg config, id uint64) window {
	w := window{id: id}
	idArg := strconv.FormatUint(id, 10)
	w.title, _ = commandOutput(ctx, cfg, "getwindowname", idArg)
	w.class, _ = commandOutput(ctx, cfg, "getwindowclassname", idArg)
	pidText, _ := commandOutput(ctx, cfg, "getwindowpid", idArg)
	w.title = strings.TrimSpace(w.title)
	w.class = strings.TrimSpace(w.class)
	w.pid, _ = strconv.Atoi(strings.TrimSpace(pidText))
	w.process = processCommandLine(w.pid)
	return w
}

func commandOutput(ctx context.Context, cfg config, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, cfg.xdotoolPath, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+cfg.display)
	output, err := cmd.Output()
	return string(output), err
}

func processCommandLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

func selectWindow(windows []window, match matcher, currentID uint64, currentPID int, excluded windowIdentity) (window, bool) {
	var candidates []window
	for _, w := range windows {
		if match.matches(w) && (w.id != excluded.id || w.pid != excluded.pid) {
			candidates = append(candidates, w)
		}
	}
	for _, candidate := range candidates {
		if candidate.id == currentID && candidate.pid == currentPID {
			return candidate, true
		}
	}
	if len(candidates) == 0 {
		return window{}, false
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id > candidates[j].id })
	return candidates[0], true
}

func containsWindow(windows []window, identity windowIdentity) bool {
	for _, w := range windows {
		if w.id == identity.id && w.pid == identity.pid {
			return true
		}
	}
	return false
}

func startCapture(ctx context.Context, cfg config, w window) (*capture, error) {
	args := []string{
		"-nostdin",
		"-loglevel", "warning",
		"-f", "x11grab",
		"-framerate", strconv.Itoa(cfg.frameRate),
		"-window_id", strconv.FormatUint(w.id, 10),
		"-i", cfg.display,
		"-vf", cfg.videoFilter,
		"-f", "v4l2",
		cfg.output,
	}
	cmd := exec.CommandContext(ctx, cfg.ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("started ffmpeg pid=%d output=%s", cmd.Process.Pid, cfg.output)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return &capture{windowID: w.id, cmd: cmd, done: done}, nil
}

func stopCapture(active *capture) {
	if active == nil || active.cmd == nil || active.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-active.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-active.done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-active.cmd.Process.Pid, syscall.SIGKILL)
		<-active.done
	}
}
