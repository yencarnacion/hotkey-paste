package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	appName             = "hotkey-paste"
	configFileName      = "config.json"
	evKey               = 0x01
	activationDebounce  = 120 * time.Millisecond
	modifierReleaseWait = 80 * time.Millisecond
	maxTypingChunkRunes = 180
	largeSnippetRunes   = 400
	typingChunkPause    = 10 * time.Millisecond
)

var (
	errXInputUnavailable  = errors.New("xinput backend unavailable")
	errEvdevUnavailable   = errors.New("evdev backend unavailable")
	errWindowsUnavailable = errors.New("windows backend unavailable")
)

var defaultDigits = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"}

var windowsShellPathCandidates = []string{
	"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe",
	"/mnt/c/Program Files/PowerShell/7/pwsh.exe",
	"/mnt/c/Program Files/PowerShell/6/pwsh.exe",
}

type config struct {
	Bindings []binding `json:"bindings"`
}

type binding struct {
	Hotkey string `json:"hotkey,omitempty"`
	Key    string `json:"key,omitempty"`
	File   string `json:"file"`
}

type runtimeBinding struct {
	Hotkey string
	File   string
	Combo  keyBinding
}

type hotkeyWatcher struct {
	bindings []runtimeBinding
}

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

type openedInput struct {
	path string
	file *os.File
}

type keyBinding struct {
	trigger      uint16
	requirements []keyRequirement
}

type keyRequirement struct {
	anyOf []uint16
}

type clipboardBackend string

const (
	clipboardBackendWayland clipboardBackend = "wayland"
	clipboardBackendX11     clipboardBackend = "x11"
)

const (
	windowsHotkeyModAlt   = 0x0001
	windowsHotkeyModCtrl  = 0x0002
	windowsHotkeyModShift = 0x0004
	windowsHotkeyModWin   = 0x0008
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "run":
		return runCommand(args)
	case "init":
		return initConfigCommand(args)
	case "init-config":
		return initConfigCommand(args)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func runCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, err := ensureConfig()
	if err != nil {
		return err
	}

	cfg, err := loadConfig(paths.configFile, paths.configDir)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	logger.Printf("starting %s", appName)
	if err := runHotkeyLoop(cfg, logger); err != nil {
		logger.Printf("stopped with error: %v", err)
		return err
	}
	logger.Printf("stopped %s", appName)
	return nil
}

func initConfigCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, err := ensureConfig()
	if err != nil {
		return err
	}

	fmt.Printf("config initialized in %s\n", paths.configDir)
	fmt.Printf("edit %s to change hotkeys or snippet files\n", paths.configFile)
	return nil
}

type appPaths struct {
	configDir  string
	configFile string
	snippetDir string
}

func ensureConfig() (appPaths, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return appPaths{}, fmt.Errorf("determine config dir: %w", err)
	}

	paths := appPaths{
		configDir:  filepath.Join(configRoot, appName),
		snippetDir: filepath.Join(configRoot, appName, "snippets"),
	}
	paths.configFile = filepath.Join(paths.configDir, configFileName)

	if err := os.MkdirAll(paths.snippetDir, 0o755); err != nil {
		return appPaths{}, fmt.Errorf("create config dirs: %w", err)
	}

	sourceSnippetDir, err := findDefaultSnippetSourceDir()
	if err != nil {
		return appPaths{}, err
	}
	if err := seedSnippetDir(paths.snippetDir, sourceSnippetDir); err != nil {
		return appPaths{}, err
	}

	if _, err := os.Stat(paths.configFile); errors.Is(err, os.ErrNotExist) {
		cfg := defaultConfig()
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return appPaths{}, fmt.Errorf("marshal default config: %w", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(paths.configFile, data, 0o644); err != nil {
			return appPaths{}, fmt.Errorf("write %s: %w", paths.configFile, err)
		}
	} else if err != nil {
		return appPaths{}, fmt.Errorf("stat %s: %w", paths.configFile, err)
	}

	return paths, nil
}

func findDefaultSnippetSourceDir() (string, error) {
	candidates := make([]string, 0, 2)

	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "snippets"))
	}
	if execPath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(execPath), "snippets"))
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate, nil
		}
	}

	if len(candidates) == 0 {
		return "", errors.New("default snippets directory not found")
	}
	return "", fmt.Errorf("default snippets directory not found; expected ./snippets or %s", filepath.Join(filepath.Dir(candidates[len(candidates)-1]), "snippets"))
}

func seedSnippetDir(destDir, sourceDir string) error {
	for _, digit := range defaultDigits {
		name := digit + ".txt"
		destPath := filepath.Join(destDir, name)
		if _, err := os.Stat(destPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", destPath, err)
		}

		sourcePath := filepath.Join(sourceDir, name)
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", sourcePath, err)
		}
		if err := os.WriteFile(destPath, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", destPath, err)
		}
	}
	return nil
}

func defaultConfig() config {
	cfg := config{Bindings: make([]binding, 0, len(defaultDigits))}
	for _, digit := range defaultDigits {
		cfg.Bindings = append(cfg.Bindings, binding{
			Hotkey: "Ctrl+Alt+" + digit,
			File:   filepath.Join("snippets", digit+".txt"),
		})
	}
	return cfg
}

func loadConfig(configPath, configDir string) (config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return config{}, fmt.Errorf("read %s: %w", configPath, err)
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if len(cfg.Bindings) == 0 {
		return config{}, fmt.Errorf("%s has no bindings", configPath)
	}

	seen := make(map[string]struct{}, len(cfg.Bindings))
	for i := range cfg.Bindings {
		b := &cfg.Bindings[i]
		if hotkeySpec(*b) == "" {
			return config{}, fmt.Errorf("missing hotkey for binding %q in %s", b.File, configPath)
		}
		if b.File == "" {
			return config{}, fmt.Errorf("missing file for hotkey %q in %s", hotkeySpec(*b), configPath)
		}

		canonical, _, err := parseHotkey(hotkeySpec(*b))
		if err != nil {
			return config{}, err
		}
		if _, ok := seen[canonical]; ok {
			return config{}, fmt.Errorf("duplicate hotkey %q in %s", canonical, configPath)
		}
		seen[canonical] = struct{}{}

		resolved, err := resolvePath(configDir, b.File)
		if err != nil {
			return config{}, err
		}
		b.File = resolved
	}

	return cfg, nil
}

func resolvePath(configDir, path string) (string, error) {
	switch {
	case strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home dir: %w", err)
		}
		return filepath.Join(home, path[2:]), nil
	case filepath.IsAbs(path):
		return path, nil
	default:
		return filepath.Join(configDir, path), nil
	}
}

func runHotkeyLoop(cfg config, logger *log.Logger) error {
	bindings, err := compileBindings(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	watcher := hotkeyWatcher{bindings: bindings}
	events := make(chan runtimeBinding, len(bindings))
	errCh := make(chan error, 1)

	go func() {
		errCh <- watcher.Watch(ctx, events)
	}()

	logger.Printf("registered %d hotkeys", len(bindings))
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case binding := <-events:
			if err := pasteSnippet(binding, logger); err != nil {
				logger.Printf("hotkey %s failed: %v", binding.Hotkey, err)
			}
		}
	}
}

func compileBindings(cfg config) ([]runtimeBinding, error) {
	result := make([]runtimeBinding, 0, len(cfg.Bindings))
	for _, entry := range cfg.Bindings {
		hotkey, combo, err := parseHotkey(hotkeySpec(entry))
		if err != nil {
			return nil, err
		}
		result = append(result, runtimeBinding{
			Hotkey: hotkey,
			File:   entry.File,
			Combo:  combo,
		})
	}
	return result, nil
}

func hotkeySpec(binding binding) string {
	if binding.Hotkey != "" {
		return binding.Hotkey
	}
	if binding.Key != "" {
		return "Ctrl+Alt+" + binding.Key
	}
	return ""
}

func parseHotkey(spec string) (string, keyBinding, error) {
	parts := strings.Split(spec, "+")
	if len(parts) == 0 {
		return "", keyBinding{}, fmt.Errorf("hotkey is empty")
	}

	canonicalParts := make([]string, 0, len(parts))
	for i := 0; i < len(parts)-1; i++ {
		token := strings.ToLower(strings.TrimSpace(parts[i]))
		switch token {
		case "ctrl", "control":
			canonicalParts = append(canonicalParts, "Ctrl")
		case "alt", "option":
			canonicalParts = append(canonicalParts, "Alt")
		case "shift":
			canonicalParts = append(canonicalParts, "Shift")
		case "super", "meta", "win", "cmd", "command":
			canonicalParts = append(canonicalParts, "Super")
		default:
			canonicalParts = append(canonicalParts, strings.TrimSpace(parts[i]))
		}
	}

	keyToken := strings.TrimSpace(parts[len(parts)-1])
	if keyToken == "" {
		return "", keyBinding{}, fmt.Errorf("missing key in hotkey %q", spec)
	}
	if len(keyToken) == 1 {
		keyToken = strings.ToUpper(keyToken)
	}

	combo, err := parseBinding(spec)
	if err != nil {
		return "", keyBinding{}, fmt.Errorf("hotkey %q: %w", spec, err)
	}

	canonicalParts = append(canonicalParts, keyToken)
	return strings.Join(canonicalParts, "+"), combo, nil
}

func (w *hotkeyWatcher) Watch(ctx context.Context, out chan<- runtimeBinding) error {
	forceBackend := strings.ToLower(strings.TrimSpace(os.Getenv("HOTKEY_PASTE_HOTKEY_BACKEND")))

	switch forceBackend {
	case "evdev":
		return w.watchEvdev(ctx, out)
	case "xinput":
		return w.watchXInput(ctx, out)
	case "windows":
		return w.watchWindows(ctx, out)
	case "windows-host":
		return w.watchWindows(ctx, out)
	}

	if windowsBackendEnabled(runtime.GOOS == "windows", runningUnderWSL()) {
		if err := w.watchWindows(ctx, out); err == nil {
			return nil
		} else if !errors.Is(err, errWindowsUnavailable) {
			return err
		} else if runtime.GOOS == "windows" {
			return err
		}
	}

	if err := w.watchXInput(ctx, out); err == nil {
		return nil
	} else if !errors.Is(err, errXInputUnavailable) {
		return err
	}

	if shouldFallbackToWindowsBackend(runtime.GOOS == "windows", runningUnderWSL(), windowsShellAvailable()) {
		if err := w.watchWindows(ctx, out); err == nil {
			return nil
		} else if !errors.Is(err, errWindowsUnavailable) {
			return err
		}
	}

	return w.watchEvdev(ctx, out)
}

func (w *hotkeyWatcher) watchWindows(ctx context.Context, out chan<- runtimeBinding) error {
	shellPath, err := findWindowsShellExecutable()
	if err != nil {
		return fmt.Errorf("%w: %v", errWindowsUnavailable, err)
	}

	var script strings.Builder
	script.WriteString(windowsHostWatcherScriptPrelude)
	bindingByID := make(map[int]runtimeBinding, len(w.bindings))
	for idx, binding := range w.bindings {
		mods, vk, err := parseWindowsHotkey(binding.Hotkey)
		if err != nil {
			return fmt.Errorf("hotkey %q is not supported by the windows backend: %w", binding.Hotkey, err)
		}
		id := idx + 1
		bindingByID[id] = binding
		script.WriteString(fmt.Sprintf(windowsHostRegisterTemplate, id, mods, vk, id, id, id, mods, vk))
	}
	script.WriteString(windowsHostWatcherScriptTail)

	cmd := exec.CommandContext(ctx, shellPath, "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encodePowerShell(script.String()))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: %v", errWindowsUnavailable, err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", errWindowsUnavailable, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256), 64*1024)
	var tail []string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		tail = appendTail(tail, line, 8)
		if !strings.HasPrefix(line, "HOTKEY:") {
			continue
		}

		id, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "HOTKEY:")))
		if err != nil {
			continue
		}

		binding, ok := bindingByID[id]
		if !ok {
			continue
		}

		select {
		case out <- binding:
		case <-ctx.Done():
			_ = cmd.Wait()
			return nil
		default:
		}
	}

	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("%w: powershell scan error: %v", errWindowsUnavailable, err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		detail := strings.TrimSpace(strings.Join(tail, " | "))
		if detail != "" {
			return fmt.Errorf("%w: powershell hotkey watcher exited: %v (%s)", errWindowsUnavailable, err, detail)
		}
		return fmt.Errorf("%w: powershell hotkey watcher exited: %v", errWindowsUnavailable, err)
	}
	return nil
}

func (w *hotkeyWatcher) watchXInput(ctx context.Context, out chan<- runtimeBinding) error {
	if strings.TrimSpace(os.Getenv("DISPLAY")) == "" {
		return fmt.Errorf("%w: DISPLAY is empty", errXInputUnavailable)
	}
	if !exists("xinput") {
		return fmt.Errorf("%w: xinput not found in PATH", errXInputUnavailable)
	}

	deviceID, err := findKeyboardDeviceID()
	if err != nil {
		return w.watchXInputXI2Root(ctx, out)
	}

	if err := w.watchXInputByDeviceID(ctx, out, deviceID); err == nil {
		return nil
	}
	return w.watchXInputXI2Root(ctx, out)
}

func (w *hotkeyWatcher) watchXInputByDeviceID(ctx context.Context, out chan<- runtimeBinding, deviceID int) error {
	cmd := exec.CommandContext(ctx, "xinput", "test", strconv.Itoa(deviceID))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: %v", errXInputUnavailable, err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", errXInputUnavailable, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256), 64*1024)

	pressedCount := map[int]int{}
	active := make([]bool, len(w.bindings))
	armed := make([]bool, len(w.bindings))
	last := make([]time.Time, len(w.bindings))
	var tail []string

	for scanner.Scan() {
		line := scanner.Text()
		tail = appendTail(tail, strings.TrimSpace(line), 8)

		code, eventKind, ok := parseXInputKeyEvent(line)
		if !ok {
			continue
		}

		updatePressedCountX11(pressedCount, code, eventKind)
		for _, binding := range w.evaluateX11(pressedCount, active, armed, last) {
			select {
			case out <- binding:
			case <-ctx.Done():
				_ = cmd.Wait()
				return nil
			default:
			}
		}
	}

	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("%w: xinput scan error: %v", errXInputUnavailable, err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		detail := strings.TrimSpace(strings.Join(tail, " | "))
		if detail != "" {
			return fmt.Errorf("%w: xinput exited: %v (%s)", errXInputUnavailable, err, detail)
		}
		return fmt.Errorf("%w: xinput exited: %v", errXInputUnavailable, err)
	}
	return nil
}

func (w *hotkeyWatcher) watchXInputXI2Root(ctx context.Context, out chan<- runtimeBinding) error {
	cmd := exec.CommandContext(ctx, "xinput", "test-xi2", "--root")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: %v", errXInputUnavailable, err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", errXInputUnavailable, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)

	pressedCount := map[int]int{}
	active := make([]bool, len(w.bindings))
	armed := make([]bool, len(w.bindings))
	last := make([]time.Time, len(w.bindings))
	var tail []string
	eventKind := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		tail = appendTail(tail, line, 8)

		if strings.HasPrefix(line, "EVENT type") {
			switch {
			case strings.Contains(line, "RawKeyPress"), strings.Contains(line, "(KeyPress)"):
				eventKind = 1
			case strings.Contains(line, "RawKeyRelease"), strings.Contains(line, "(KeyRelease)"):
				eventKind = -1
			default:
				eventKind = 0
			}
			continue
		}

		if eventKind == 0 || !strings.HasPrefix(line, "detail:") {
			continue
		}

		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "detail:")))
		if len(fields) == 0 {
			continue
		}

		code, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		updatePressedCountX11(pressedCount, code, eventKind)
		for _, binding := range w.evaluateX11(pressedCount, active, armed, last) {
			select {
			case out <- binding:
			case <-ctx.Done():
				_ = cmd.Wait()
				return nil
			default:
			}
		}
	}

	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("%w: xinput xi2 scan error: %v", errXInputUnavailable, err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		detail := strings.TrimSpace(strings.Join(tail, " | "))
		if detail != "" {
			return fmt.Errorf("%w: xinput xi2 exited: %v (%s)", errXInputUnavailable, err, detail)
		}
		return fmt.Errorf("%w: xinput xi2 exited: %v", errXInputUnavailable, err)
	}
	return nil
}

func (w *hotkeyWatcher) watchEvdev(ctx context.Context, out chan<- runtimeBinding) error {
	paths, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("%w: no /dev/input/event* devices found", errEvdevUnavailable)
	}

	inputs := make([]openedInput, 0, len(paths))
	for _, path := range paths {
		supports, err := deviceSupportsAnyBinding(path, w.bindings)
		if err != nil || !supports {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		inputs = append(inputs, openedInput{path: path, file: f})
	}
	if len(inputs) == 0 {
		return fmt.Errorf("%w: unable to read keyboard devices; install xinput or add user to input group", errEvdevUnavailable)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(inputs))
	var stateMu sync.Mutex
	pressedCount := map[uint16]int{}
	active := make([]bool, len(w.bindings))
	armed := make([]bool, len(w.bindings))
	last := make([]time.Time, len(w.bindings))

	for _, in := range inputs {
		wg.Add(1)
		go func(file *os.File) {
			defer wg.Done()
			for {
				var event inputEvent
				if err := binary.Read(file, binary.LittleEndian, &event); err != nil {
					if err == io.EOF {
						return
					}
					errCh <- err
					return
				}
				if event.Type != evKey || event.Value == 2 {
					continue
				}

				stateMu.Lock()
				updatePressedCountEvdev(pressedCount, event.Code, event.Value)
				fired := w.evaluateEvdev(pressedCount, active, armed, last)
				stateMu.Unlock()

				for _, binding := range fired {
					select {
					case out <- binding:
					case <-ctx.Done():
						return
					default:
					}
				}
			}
		}(in.file)
	}

	select {
	case <-ctx.Done():
		for _, in := range inputs {
			_ = in.file.Close()
		}
		wg.Wait()
		return nil
	case err := <-errCh:
		for _, in := range inputs {
			_ = in.file.Close()
		}
		wg.Wait()
		return err
	}
}

func (w *hotkeyWatcher) evaluateX11(pressedCount map[int]int, active, armed []bool, last []time.Time) []runtimeBinding {
	now := time.Now()
	fired := make([]runtimeBinding, 0, 1)
	for i, binding := range w.bindings {
		nowActive := bindingActiveX11(binding.Combo, pressedCount)
		if nowActive && !active[i] {
			armed[i] = true
		}
		active[i] = nowActive
		if nowActive || !armed[i] || !bindingReleasedX11(binding.Combo, pressedCount) {
			continue
		}
		armed[i] = false
		if !last[i].IsZero() && now.Sub(last[i]) < activationDebounce {
			continue
		}
		last[i] = now
		fired = append(fired, binding)
	}
	return fired
}

func (w *hotkeyWatcher) evaluateEvdev(pressedCount map[uint16]int, active, armed []bool, last []time.Time) []runtimeBinding {
	now := time.Now()
	fired := make([]runtimeBinding, 0, 1)
	for i, binding := range w.bindings {
		nowActive := bindingActiveEvdev(binding.Combo, pressedCount)
		if nowActive && !active[i] {
			armed[i] = true
		}
		active[i] = nowActive
		if nowActive || !armed[i] || !bindingReleasedEvdev(binding.Combo, pressedCount) {
			continue
		}
		armed[i] = false
		if !last[i].IsZero() && now.Sub(last[i]) < activationDebounce {
			continue
		}
		last[i] = now
		fired = append(fired, binding)
	}
	return fired
}

func bindingActiveX11(binding keyBinding, pressedCount map[int]int) bool {
	if pressedCount[x11Code(binding.trigger)] == 0 {
		return false
	}
	for _, req := range binding.requirements {
		match := false
		for _, code := range req.anyOf {
			if pressedCount[x11Code(code)] > 0 {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func bindingReleasedX11(binding keyBinding, pressedCount map[int]int) bool {
	if pressedCount[x11Code(binding.trigger)] > 0 {
		return false
	}
	for _, req := range binding.requirements {
		for _, code := range req.anyOf {
			if pressedCount[x11Code(code)] > 0 {
				return false
			}
		}
	}
	return true
}

func bindingActiveEvdev(binding keyBinding, pressedCount map[uint16]int) bool {
	if pressedCount[binding.trigger] == 0 {
		return false
	}
	for _, req := range binding.requirements {
		match := false
		for _, code := range req.anyOf {
			if pressedCount[code] > 0 {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func bindingReleasedEvdev(binding keyBinding, pressedCount map[uint16]int) bool {
	if pressedCount[binding.trigger] > 0 {
		return false
	}
	for _, req := range binding.requirements {
		for _, code := range req.anyOf {
			if pressedCount[code] > 0 {
				return false
			}
		}
	}
	return true
}

func updatePressedCountX11(pressedCount map[int]int, code, eventKind int) {
	if eventKind == 1 {
		pressedCount[code]++
		return
	}
	if pressedCount[code] > 0 {
		pressedCount[code]--
		if pressedCount[code] == 0 {
			delete(pressedCount, code)
		}
	}
}

func updatePressedCountEvdev(pressedCount map[uint16]int, code uint16, value int32) {
	if value == 1 {
		pressedCount[code]++
		return
	}
	if pressedCount[code] > 0 {
		pressedCount[code]--
		if pressedCount[code] == 0 {
			delete(pressedCount, code)
		}
	}
}

func pasteSnippet(binding runtimeBinding, logger *log.Logger) error {
	data, err := os.ReadFile(binding.File)
	if err != nil {
		return fmt.Errorf("read %s: %w", binding.File, err)
	}
	if len(data) == 0 {
		logger.Printf("hotkey %s mapped to empty file %s", binding.Hotkey, binding.File)
		return nil
	}

	time.Sleep(modifierReleaseWait)
	if err := injectText(string(data)); err != nil {
		return err
	}

	logger.Printf("pasted snippet for %s", binding.Hotkey)
	return nil
}

func injectText(text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")

	method, err := resolveOutputMethod(text)
	if err != nil {
		return err
	}

	switch method {
	case "clipboard":
		return typeViaClipboard(text)
	case "windows":
		return typeViaWindowsHost(text)
	case "wtype", "xdotool":
		return typeChunked(method, text)
	default:
		return fmt.Errorf("unsupported output method %q", method)
	}
}

func resolveOutputMethod(text string) (string, error) {
	method := strings.ToLower(strings.TrimSpace(os.Getenv("HOTKEY_PASTE_OUTPUT_METHOD")))
	return resolveOutputMethodWith(text, method, runningUnderWSL(), runtime.GOOS == "windows", windowsShellAvailable(), exists)
}

func resolveOutputMethodWith(text, method string, underWSL, onWindows, hasWindowsShell bool, commandExists func(string) bool) (string, error) {
	if method == "" || method == "auto" {
		if shouldAutoUseWindowsBackend(onWindows, underWSL, hasWindowsShell) {
			return "windows", nil
		}
		if onWindows {
			return "", errors.New("no supported paste backend found; install powershell.exe or pwsh.exe")
		}
		if utf8.RuneCountInString(text) > largeSnippetRunes {
			if _, err := resolveClipboardBackendWith(commandExists); err == nil {
				return "clipboard", nil
			}
		}

		sessionType := strings.ToLower(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE")))
		isWayland := sessionType == "wayland" || strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != ""
		isX11 := sessionType == "x11" || strings.TrimSpace(os.Getenv("DISPLAY")) != ""

		if isWayland && commandExists("wtype") {
			return "wtype", nil
		}
		if isX11 && commandExists("xdotool") {
			return "xdotool", nil
		}
		if commandExists("wtype") {
			return "wtype", nil
		}
		if commandExists("xdotool") {
			return "xdotool", nil
		}
		if _, err := resolveClipboardBackendWith(commandExists); err == nil {
			return "clipboard", nil
		}
		return "", errors.New("no supported paste backend found; install powershell.exe, xdotool, wtype, or clipboard tools")
	}

	switch method {
	case "clipboard":
		if _, err := resolveClipboardBackendWith(commandExists); err != nil {
			return "", err
		}
		return method, nil
	case "windows":
		if !shouldAutoUseWindowsBackend(onWindows, underWSL, hasWindowsShell) && !windowsBackendEnabled(onWindows, underWSL) {
			return "", errors.New("windows output method requires Windows, WSL, or a reachable Windows PowerShell from a headless Linux session")
		}
		if !hasWindowsShell {
			return "", errors.New("powershell.exe or pwsh.exe not found")
		}
		return method, nil
	case "wtype":
		if !commandExists("wtype") {
			return "", errors.New("wtype not found in PATH")
		}
		return method, nil
	case "xdotool":
		if !commandExists("xdotool") {
			return "", errors.New("xdotool not found in PATH")
		}
		return method, nil
	default:
		return "", fmt.Errorf("invalid HOTKEY_PASTE_OUTPUT_METHOD %q", method)
	}
}

func typeChunked(method, text string) error {
	chunks := splitTextByRuneCount(text, maxTypingChunkRunes)
	longText := len(chunks) > 1

	for idx, chunk := range chunks {
		if err := typeChunk(method, chunk, longText); err != nil {
			return err
		}
		if longText && idx < len(chunks)-1 {
			time.Sleep(typingChunkPause)
		}
	}
	return nil
}

func typeChunk(method, chunk string, longText bool) error {
	switch method {
	case "wtype":
		out, err := exec.Command("wtype", chunk).CombinedOutput()
		if err != nil {
			return commandError("wtype", err, out)
		}
		return nil
	case "xdotool":
		delay := "0"
		if longText {
			delay = "1"
		}
		out, err := exec.Command("xdotool", "type", "--clearmodifiers", "--delay", delay, chunk).CombinedOutput()
		if err != nil {
			return commandError("xdotool", err, out)
		}
		return nil
	default:
		return fmt.Errorf("unsupported output method %q", method)
	}
}

func splitTextByRuneCount(text string, maxRunes int) []string {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}

	chunks := make([]string, 0, (utf8.RuneCountInString(text)+maxRunes-1)/maxRunes)
	start := 0
	runeCount := 0

	for idx := range text {
		if runeCount == maxRunes {
			chunks = append(chunks, text[start:idx])
			start = idx
			runeCount = 0
		}
		runeCount++
	}
	chunks = append(chunks, text[start:])
	return chunks
}

func typeViaClipboard(text string) error {
	backend, err := resolveClipboardBackend()
	if err != nil {
		return err
	}

	previousClipboard, hasPreviousClipboard := readClipboardBestEffort(backend)
	if err := writeClipboard(backend, text); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)

	if err := triggerClipboardPaste(backend); err != nil {
		if hasPreviousClipboard {
			_ = writeClipboard(backend, previousClipboard)
		}
		return err
	}

	if hasPreviousClipboard {
		time.Sleep(100 * time.Millisecond)
		_ = writeClipboard(backend, previousClipboard)
	}
	return nil
}

func resolveClipboardBackend() (clipboardBackend, error) {
	return resolveClipboardBackendWith(exists)
}

func resolveClipboardBackendWith(commandExists func(string) bool) (clipboardBackend, error) {
	sessionType := strings.ToLower(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE")))
	isWayland := sessionType == "wayland" || strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != ""
	isX11 := sessionType == "x11" || strings.TrimSpace(os.Getenv("DISPLAY")) != ""

	hasWaylandClipboard := commandExists("wl-copy")
	hasWaylandPasteKey := commandExists("wtype")
	hasX11Clipboard := commandExists("xclip")
	hasX11PasteKey := commandExists("xdotool")

	if isWayland {
		if hasWaylandClipboard && hasWaylandPasteKey {
			return clipboardBackendWayland, nil
		}
		if isX11 && hasX11Clipboard && hasX11PasteKey {
			return clipboardBackendX11, nil
		}
		return "", errors.New("clipboard mode requires wl-copy+wtype on Wayland or xclip+xdotool on XWayland")
	}

	if isX11 {
		if hasX11Clipboard && hasX11PasteKey {
			return clipboardBackendX11, nil
		}
		return "", errors.New("clipboard mode requires xclip+xdotool on X11")
	}

	if hasWaylandClipboard && hasWaylandPasteKey {
		return clipboardBackendWayland, nil
	}
	if hasX11Clipboard && hasX11PasteKey {
		return clipboardBackendX11, nil
	}

	return "", errors.New("clipboard mode requires wl-copy+wtype or xclip+xdotool")
}

func typeViaWindowsHost(text string) error {
	shellPath, err := findWindowsShellExecutable()
	if err != nil {
		return err
	}

	cmd := exec.Command(shellPath, "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encodePowerShell(windowsHostPasteScript))
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return commandError("powershell.exe", err, out)
	}
	return nil
}

func readClipboardBestEffort(backend clipboardBackend) (string, bool) {
	cmd := clipboardReadCommand(backend)
	if cmd == nil {
		return "", false
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func writeClipboard(backend clipboardBackend, text string) error {
	cmd := clipboardWriteCommand(backend)
	if cmd == nil {
		return fmt.Errorf("clipboard backend %q not supported", backend)
	}
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write clipboard: %w", err)
	}
	return nil
}

func triggerClipboardPaste(backend clipboardBackend) error {
	cmd := clipboardPasteCommand(backend)
	if cmd == nil {
		return fmt.Errorf("clipboard backend %q not supported", backend)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return commandError("clipboard paste", err, out)
	}
	return nil
}

func clipboardReadCommand(backend clipboardBackend) *exec.Cmd {
	switch backend {
	case clipboardBackendWayland:
		if !exists("wl-paste") {
			return nil
		}
		return exec.Command("wl-paste")
	case clipboardBackendX11:
		return exec.Command("xclip", "-selection", "clipboard", "-o")
	default:
		return nil
	}
}

func clipboardWriteCommand(backend clipboardBackend) *exec.Cmd {
	switch backend {
	case clipboardBackendWayland:
		return exec.Command("wl-copy")
	case clipboardBackendX11:
		return exec.Command("xclip", "-selection", "clipboard", "-in")
	default:
		return nil
	}
}

func clipboardPasteCommand(backend clipboardBackend) *exec.Cmd {
	switch backend {
	case clipboardBackendWayland:
		return exec.Command("wtype", "-M", "ctrl", "v", "-m", "ctrl")
	case clipboardBackendX11:
		return exec.Command("xdotool", "key", "--clearmodifiers", "ctrl+v")
	default:
		return nil
	}
}

func commandError(name string, err error, out []byte) error {
	trimmed := strings.TrimSpace(string(bytes.TrimSpace(out)))
	if trimmed == "" {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return fmt.Errorf("%s failed: %w (%s)", name, err, trimmed)
}

func exists(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func findKeyboardDeviceID() (int, error) {
	out, err := exec.Command("xinput", "list", "--short").Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(out), "\n")

	for _, line := range lines {
		if !strings.Contains(strings.ToLower(line), "slave keyboard") {
			continue
		}
		if id, ok := parseXInputDeviceID(line); ok {
			return id, nil
		}
	}

	for _, line := range lines {
		if !strings.Contains(strings.ToLower(line), "master keyboard") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "virtual core keyboard") {
			continue
		}
		if id, ok := parseXInputDeviceID(line); ok {
			return id, nil
		}
	}

	for _, line := range lines {
		if !strings.Contains(strings.ToLower(line), "master keyboard") {
			continue
		}
		if id, ok := parseXInputDeviceID(line); ok {
			return id, nil
		}
	}

	return 0, errors.New("unable to find a keyboard device from xinput")
}

func appendTail(lines []string, line string, max int) []string {
	if line == "" || max <= 0 {
		return lines
	}
	lines = append(lines, line)
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines
}

func parseXInputDeviceID(line string) (int, bool) {
	start := strings.Index(line, "id=")
	if start < 0 {
		return 0, false
	}
	fields := strings.Fields(line[start+3:])
	if len(fields) == 0 {
		return 0, false
	}
	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}
	return id, true
}

func parseXInputKeyEvent(line string) (int, int, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 3 || !strings.EqualFold(fields[0], "key") {
		return 0, 0, false
	}

	eventKind := 0
	if strings.EqualFold(fields[1], "press") {
		eventKind = 1
	} else if strings.EqualFold(fields[1], "release") {
		eventKind = -1
	}
	if eventKind == 0 {
		return 0, 0, false
	}

	code, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return code, eventKind, true
}

func x11Code(evdevCode uint16) int {
	return int(evdevCode) + 8
}

func deviceSupportsAnyBinding(eventPath string, bindings []runtimeBinding) (bool, error) {
	bits, bitsPerWord, err := readKeyBitmap(eventPath)
	if err != nil {
		return false, err
	}
	for _, binding := range bindings {
		if bindingSupported(binding.Combo, bits, bitsPerWord) {
			return true, nil
		}
	}
	return false, nil
}

func bindingSupported(binding keyBinding, bitmap []uint64, bitsPerWord int) bool {
	if !hasKeyCode(bitmap, bitsPerWord, binding.trigger) {
		return false
	}
	for _, req := range binding.requirements {
		match := false
		for _, code := range req.anyOf {
			if hasKeyCode(bitmap, bitsPerWord, code) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func readKeyBitmap(eventPath string) ([]uint64, int, error) {
	eventName := filepath.Base(eventPath)
	capsPath := filepath.Join("/sys/class/input", eventName, "device", "capabilities", "key")
	raw, err := os.ReadFile(capsPath)
	if err != nil {
		return nil, 0, err
	}
	words := strings.Fields(strings.TrimSpace(string(raw)))
	if len(words) == 0 {
		return nil, 0, fmt.Errorf("empty key capability bitmap for %s", eventName)
	}

	bitsPerWord := 64
	if len(words[0]) <= 8 {
		bitsPerWord = 32
	}

	out := make([]uint64, 0, len(words))
	for _, word := range words {
		value, err := strconv.ParseUint(word, 16, 64)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, value)
	}
	return out, bitsPerWord, nil
}

func hasKeyCode(bitmap []uint64, bitsPerWord int, code uint16) bool {
	idx := int(code) / bitsPerWord
	if idx < 0 || idx >= len(bitmap) {
		return false
	}
	bit := uint(code) % uint(bitsPerWord)
	mask := uint64(1) << bit
	if bitmap[idx]&mask != 0 {
		return true
	}
	revIdx := len(bitmap) - 1 - idx
	return revIdx >= 0 && revIdx < len(bitmap) && bitmap[revIdx]&mask != 0
}

func parseBinding(raw string) (keyBinding, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return keyBinding{}, errors.New("hotkey is empty")
	}

	parts := strings.Split(s, "+")
	if len(parts) == 1 {
		trigger, err := parseKeyCode(parts[0])
		if err != nil {
			return keyBinding{}, err
		}
		return keyBinding{trigger: trigger}, nil
	}

	requirements := make([]keyRequirement, 0, len(parts)-1)
	for i := 0; i < len(parts)-1; i++ {
		token := strings.TrimSpace(parts[i])
		if token == "" {
			return keyBinding{}, fmt.Errorf("invalid hotkey %q", raw)
		}
		if codes, ok := modifierAliases[token]; ok {
			requirements = append(requirements, keyRequirement{anyOf: codes})
			continue
		}
		code, err := parseKeyCode(token)
		if err != nil {
			return keyBinding{}, err
		}
		requirements = append(requirements, keyRequirement{anyOf: []uint16{code}})
	}

	triggerToken := strings.TrimSpace(parts[len(parts)-1])
	if triggerToken == "" {
		return keyBinding{}, fmt.Errorf("invalid hotkey %q", raw)
	}
	trigger, err := parseKeyCode(triggerToken)
	if err != nil {
		return keyBinding{}, err
	}
	return keyBinding{trigger: trigger, requirements: requirements}, nil
}

func parseKeyCode(raw string) (uint16, error) {
	s := normalizeKeyToken(raw)
	if s == "" {
		return 0, errors.New("hotkey is empty")
	}

	if strings.HasPrefix(s, "KEY_") {
		if code, ok := keyNameMap[s]; ok {
			return code, nil
		}
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 65535 {
			return 0, fmt.Errorf("invalid numeric key code %d", n)
		}
		return uint16(n), nil
	}

	return 0, fmt.Errorf("unsupported hotkey %q", raw)
}

func normalizeKeyToken(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if s == "$" {
		return "KEY_4"
	}
	if s == `\` {
		return "KEY_BACKSLASH"
	}
	if len(s) == 1 && s[0] >= 'A' && s[0] <= 'Z' {
		return "KEY_" + s
	}
	if len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
		return "KEY_" + s
	}
	if strings.HasPrefix(s, "F") {
		if _, err := strconv.Atoi(s[1:]); err == nil {
			return "KEY_" + s
		}
	}
	if _, err := strconv.Atoi(s); err == nil {
		return s
	}
	if !strings.HasPrefix(s, "KEY_") {
		return "KEY_" + s
	}
	return s
}

func runningUnderWSL() bool {
	if runningUnderWSLFrom(os.Getenv("WSL_DISTRO_NAME"), os.Getenv("WSL_INTEROP"), readProcFile("/proc/sys/kernel/osrelease"), readProcFile("/proc/version")) {
		return true
	}
	return fileExists("/proc/sys/fs/binfmt_misc/WSLInterop") || fileExists("/run/WSL")
}

func runningUnderWSLFrom(wslDistro, wslInterop, kernelRelease, procVersion string) bool {
	if strings.TrimSpace(wslDistro) != "" || strings.TrimSpace(wslInterop) != "" {
		return true
	}

	release := strings.ToLower(kernelRelease)
	version := strings.ToLower(procVersion)
	return strings.Contains(release, "microsoft") || strings.Contains(version, "microsoft")
}

func windowsBackendEnabled(onWindows, underWSL bool) bool {
	return onWindows || underWSL
}

func shouldFallbackToWindowsBackend(onWindows, underWSL, hasWindowsShell bool) bool {
	return !windowsBackendEnabled(onWindows, underWSL) && hasWindowsShell && headlessLinuxSession()
}

func shouldAutoUseWindowsBackend(onWindows, underWSL, hasWindowsShell bool) bool {
	if !hasWindowsShell {
		return false
	}
	return windowsBackendEnabled(onWindows, underWSL) || headlessLinuxSession()
}

func headlessLinuxSession() bool {
	return runtime.GOOS != "windows" &&
		strings.TrimSpace(os.Getenv("DISPLAY")) == "" &&
		strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) == ""
}

func windowsShellAvailable() bool {
	_, err := findWindowsShellExecutable()
	return err == nil
}

func findWindowsShellExecutable() (string, error) {
	for _, name := range []string{"powershell.exe", "pwsh.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	for _, path := range windowsShellPathCandidates {
		if fileExists(path) {
			return path, nil
		}
	}
	return "", errors.New("powershell.exe or pwsh.exe not found in PATH or common WSL mount locations")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readProcFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseWindowsHotkey(spec string) (uint32, uint32, error) {
	parts := strings.Split(spec, "+")
	if len(parts) == 0 {
		return 0, 0, errors.New("hotkey is empty")
	}

	var mods uint32
	for _, part := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "ctrl", "control":
			mods |= windowsHotkeyModCtrl
		case "alt", "option":
			mods |= windowsHotkeyModAlt
		case "shift":
			mods |= windowsHotkeyModShift
		case "super", "meta", "win", "cmd", "command":
			mods |= windowsHotkeyModWin
		default:
			return 0, 0, fmt.Errorf("unsupported modifier %q", part)
		}
	}

	vk, err := windowsVirtualKey(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil {
		return 0, 0, err
	}
	return mods, vk, nil
}

func windowsVirtualKey(token string) (uint32, error) {
	key := strings.ToUpper(strings.TrimSpace(token))
	if key == "" {
		return 0, errors.New("missing key in hotkey")
	}
	if len(key) == 1 {
		switch {
		case key[0] >= '0' && key[0] <= '9':
			return uint32(key[0]), nil
		case key[0] >= 'A' && key[0] <= 'Z':
			return uint32(key[0]), nil
		}
	}
	if strings.HasPrefix(key, "F") {
		n, err := strconv.Atoi(key[1:])
		if err == nil && n >= 1 && n <= 24 {
			return uint32(0x6F + n), nil
		}
	}

	switch key {
	case `\`:
		return 0xDC, nil
	case "BACKSLASH":
		return 0xDC, nil
	case "CAPSLOCK":
		return 0x14, nil
	default:
		return 0, fmt.Errorf("unsupported key %q", token)
	}
}

func encodePowerShell(script string) string {
	utf16 := make([]byte, 0, len(script)*2)
	for _, r := range script {
		utf16 = append(utf16, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(utf16)
}

const windowsHostWatcherScriptPrelude = `$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class HotkeyNative {
    [StructLayout(LayoutKind.Sequential)]
    public struct POINT {
        public int X;
        public int Y;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct MSG {
        public IntPtr hwnd;
        public uint message;
        public UIntPtr wParam;
        public IntPtr lParam;
        public uint time;
        public POINT pt;
    }

    [DllImport("user32.dll", SetLastError=true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool RegisterHotKey(IntPtr hWnd, int id, uint fsModifiers, uint vk);

    [DllImport("user32.dll", SetLastError=true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool UnregisterHotKey(IntPtr hWnd, int id);

    [DllImport("user32.dll")]
    public static extern int GetMessage(out MSG lpMsg, IntPtr hWnd, uint wMsgFilterMin, uint wMsgFilterMax);

    [DllImport("user32.dll")]
    public static extern short GetAsyncKeyState(int vKey);
}
"@
$registered = New-Object System.Collections.Generic.List[int]
$hotkeys = @{}

function Test-KeyDown([uint32]$vk) {
    return (([HotkeyNative]::GetAsyncKeyState([int]$vk) -band 0x8000) -ne 0)
}

function Wait-HotkeyRelease([uint32]$mods, [uint32]$vk) {
    while ($true) {
        $down = $false
        if ((($mods -band 0x0002) -ne 0) -and (Test-KeyDown 0x11)) { $down = $true }
        if ((($mods -band 0x0004) -ne 0) -and (Test-KeyDown 0x10)) { $down = $true }
        if ((($mods -band 0x0001) -ne 0) -and (Test-KeyDown 0x12)) { $down = $true }
        if ((($mods -band 0x0008) -ne 0) -and ((Test-KeyDown 0x5B) -or (Test-KeyDown 0x5C))) { $down = $true }
        if (Test-KeyDown $vk) { $down = $true }
        if (-not $down) {
            break
        }
        Start-Sleep -Milliseconds 10
    }
}
try {
`

const windowsHostRegisterTemplate = `    if (-not [HotkeyNative]::RegisterHotKey([IntPtr]::Zero, %d, %d, %d)) {
        $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
        throw ('failed to register hotkey id %d (win32=' + $code + ')')
    }
    $registered.Add(%d) | Out-Null
    $hotkeys[%d] = @{ Mods = [uint32]%d; Vk = [uint32]%d }
`

const windowsHostWatcherScriptTail = `    $msg = New-Object HotkeyNative+MSG
    while ([HotkeyNative]::GetMessage([ref]$msg, [IntPtr]::Zero, 0, 0) -gt 0) {
        if ($msg.message -eq 0x0312) {
            $id = [int]$msg.wParam.ToUInt32()
            if ($hotkeys.ContainsKey($id)) {
                $entry = $hotkeys[$id]
                Wait-HotkeyRelease $entry.Mods $entry.Vk
            }
            [Console]::Out.WriteLine(('HOTKEY:{0}' -f $id))
        }
    }
} finally {
    foreach ($id in $registered) {
        [HotkeyNative]::UnregisterHotKey([IntPtr]::Zero, $id) | Out-Null
    }
}
`

const windowsHostPasteScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
$text = [Console]::In.ReadToEnd()
if ([string]::IsNullOrEmpty($text)) {
    exit 0
}
$hasPrevious = $true
try {
    $previous = Get-Clipboard -Raw
} catch {
    $hasPrevious = $false
}
Set-Clipboard -Value $text
Start-Sleep -Milliseconds 50
[System.Windows.Forms.SendKeys]::SendWait('^v')
if ($hasPrevious) {
    Start-Sleep -Milliseconds 100
    Set-Clipboard -Value $previous
}
`

var keyNameMap = map[string]uint16{
	"KEY_0":          11,
	"KEY_1":          2,
	"KEY_2":          3,
	"KEY_3":          4,
	"KEY_4":          5,
	"KEY_5":          6,
	"KEY_6":          7,
	"KEY_7":          8,
	"KEY_8":          9,
	"KEY_9":          10,
	"KEY_A":          30,
	"KEY_B":          48,
	"KEY_C":          46,
	"KEY_D":          32,
	"KEY_E":          18,
	"KEY_F":          33,
	"KEY_G":          34,
	"KEY_H":          35,
	"KEY_I":          23,
	"KEY_J":          36,
	"KEY_K":          37,
	"KEY_L":          38,
	"KEY_M":          50,
	"KEY_N":          49,
	"KEY_O":          24,
	"KEY_P":          25,
	"KEY_Q":          16,
	"KEY_R":          19,
	"KEY_S":          31,
	"KEY_T":          20,
	"KEY_U":          22,
	"KEY_V":          47,
	"KEY_W":          17,
	"KEY_X":          45,
	"KEY_Y":          21,
	"KEY_Z":          44,
	"KEY_BACKSLASH":  43,
	"KEY_F1":         59,
	"KEY_F2":         60,
	"KEY_F3":         61,
	"KEY_F4":         62,
	"KEY_F5":         63,
	"KEY_F6":         64,
	"KEY_F7":         65,
	"KEY_F8":         66,
	"KEY_F9":         67,
	"KEY_F10":        68,
	"KEY_F11":        87,
	"KEY_F12":        88,
	"KEY_LEFTCTRL":   29,
	"KEY_RIGHTCTRL":  97,
	"KEY_LEFTALT":    56,
	"KEY_RIGHTALT":   100,
	"KEY_LEFTSHIFT":  42,
	"KEY_RIGHTSHIFT": 54,
	"KEY_CAPSLOCK":   58,
	"KEY_LEFTMETA":   125,
	"KEY_RIGHTMETA":  126,
}

var modifierAliases = map[string][]uint16{
	"CTRL":    {29, 97},
	"CONTROL": {29, 97},
	"ALT":     {56, 100},
	"OPTION":  {56, 100},
	"SHIFT":   {42, 54},
	"META":    {125, 126},
	"SUPER":   {125, 126},
	"WIN":     {125, 126},
	"CMD":     {125, 126},
	"COMMAND": {125, 126},
}
