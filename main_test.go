package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseHotkeyCtrlAltDigit(t *testing.T) {
	canonical, combo, err := parseHotkey("Ctrl+Alt+1")
	if err != nil {
		t.Fatalf("parseHotkey returned error: %v", err)
	}

	if canonical != "Ctrl+Alt+1" {
		t.Fatalf("unexpected canonical hotkey: %q", canonical)
	}

	want := keyBinding{
		trigger: 2,
		requirements: []keyRequirement{
			{anyOf: []uint16{29, 97}},
			{anyOf: []uint16{56, 100}},
		},
	}
	if !reflect.DeepEqual(combo, want) {
		t.Fatalf("unexpected combo: %#v", combo)
	}
}

func TestSplitTextByRuneCount(t *testing.T) {
	chunks := splitTextByRuneCount("abcdef", 2)
	want := []string{"ab", "cd", "ef"}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestBindingActiveEvdev(t *testing.T) {
	combo, err := parseBinding("Ctrl+Alt+1")
	if err != nil {
		t.Fatalf("parseBinding returned error: %v", err)
	}

	pressed := map[uint16]int{
		29: 1,
		56: 1,
		2:  1,
	}
	if !bindingActiveEvdev(combo, pressed) {
		t.Fatal("expected combo to be active")
	}

	delete(pressed, 56)
	if bindingActiveEvdev(combo, pressed) {
		t.Fatal("expected combo to be inactive without Alt")
	}
}

func TestEvaluateEvdevFiresOnRelease(t *testing.T) {
	combo, err := parseBinding("Ctrl+Shift+2")
	if err != nil {
		t.Fatalf("parseBinding returned error: %v", err)
	}

	w := hotkeyWatcher{
		bindings: []runtimeBinding{
			{Hotkey: "Ctrl+Shift+2", Combo: combo},
		},
	}

	pressed := map[uint16]int{}
	active := make([]bool, 1)
	armed := make([]bool, 1)
	last := make([]time.Time, 1)

	updatePressedCountEvdev(pressed, 29, 1)
	if got := w.evaluateEvdev(pressed, active, armed, last); len(got) != 0 {
		t.Fatalf("unexpected activation before full combo: %#v", got)
	}

	updatePressedCountEvdev(pressed, 42, 1)
	if got := w.evaluateEvdev(pressed, active, armed, last); len(got) != 0 {
		t.Fatalf("unexpected activation before trigger press: %#v", got)
	}

	updatePressedCountEvdev(pressed, 3, 1)
	if got := w.evaluateEvdev(pressed, active, armed, last); len(got) != 0 {
		t.Fatalf("expected no activation while combo is still held, got %#v", got)
	}

	updatePressedCountEvdev(pressed, 3, 0)
	got := w.evaluateEvdev(pressed, active, armed, last)
	if len(got) != 1 || got[0].Hotkey != "Ctrl+Shift+2" {
		t.Fatalf("expected release activation for hotkey, got %#v", got)
	}

	if got := w.evaluateEvdev(pressed, active, armed, last); len(got) != 0 {
		t.Fatalf("unexpected repeated activation after release: %#v", got)
	}
}

func TestRunningUnderWSLFrom(t *testing.T) {
	if !runningUnderWSLFrom("Ubuntu", "", "", "") {
		t.Fatal("expected WSL_DISTRO_NAME to mark WSL environment")
	}
	if !runningUnderWSLFrom("", "", "5.15.153.1-microsoft-standard-WSL2", "") {
		t.Fatal("expected microsoft kernel release to mark WSL environment")
	}
	if runningUnderWSLFrom("", "", "6.8.0-generic", "Linux version 6.8.0-generic") {
		t.Fatal("did not expect plain Linux environment to be marked as WSL")
	}
}

func TestParseWindowsHotkeyCtrlAltDigit(t *testing.T) {
	mods, vk, err := parseWindowsHotkey("Ctrl+Alt+1")
	if err != nil {
		t.Fatalf("parseWindowsHotkey returned error: %v", err)
	}
	if mods != windowsHotkeyModCtrl|windowsHotkeyModAlt {
		t.Fatalf("unexpected modifiers: %d", mods)
	}
	if vk != uint32('1') {
		t.Fatalf("unexpected virtual key: %d", vk)
	}
}

func TestResolveOutputMethodWithPrefersWindowsInWSL(t *testing.T) {
	t.Setenv("XDG_SESSION_TYPE", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")

	method, err := resolveOutputMethodWith("hello", "auto", true, false, func(name string) bool {
		return name == "powershell.exe"
	})
	if err != nil {
		t.Fatalf("resolveOutputMethodWith returned error: %v", err)
	}
	if method != "windows" {
		t.Fatalf("unexpected method: %q", method)
	}
}

func TestResolveOutputMethodWithPrefersWindowsOnNativeWindows(t *testing.T) {
	t.Setenv("XDG_SESSION_TYPE", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")

	method, err := resolveOutputMethodWith("hello", "auto", false, true, func(name string) bool {
		return name == "powershell.exe"
	})
	if err != nil {
		t.Fatalf("resolveOutputMethodWith returned error: %v", err)
	}
	if method != "windows" {
		t.Fatalf("unexpected method: %q", method)
	}
}

func TestSeedSnippetDirCopiesSourceContent(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "snippets")
	destDir := filepath.Join(t.TempDir(), "snippets")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	for _, digit := range defaultDigits {
		content := "snippet-" + digit
		if err := os.WriteFile(filepath.Join(sourceDir, digit+".txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write source snippet: %v", err)
		}
	}

	if err := seedSnippetDir(destDir, sourceDir); err != nil {
		t.Fatalf("seedSnippetDir returned error: %v", err)
	}

	for _, digit := range defaultDigits {
		data, err := os.ReadFile(filepath.Join(destDir, digit+".txt"))
		if err != nil {
			t.Fatalf("read dest snippet: %v", err)
		}
		if string(data) != "snippet-"+digit {
			t.Fatalf("unexpected dest content for %s: %q", digit, string(data))
		}
	}
}

func TestRunInitSeedsConfigFromLocalSnippets(t *testing.T) {
	tempDir := t.TempDir()
	snippetDir := filepath.Join(tempDir, "snippets")
	if err := os.MkdirAll(snippetDir, 0o755); err != nil {
		t.Fatalf("mkdir snippets: %v", err)
	}

	for _, digit := range defaultDigits {
		content := "from-local-" + digit
		if err := os.WriteFile(filepath.Join(snippetDir, digit+".txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write source snippet: %v", err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(oldWD)
	}()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config-home"))
	if err := run([]string{"init"}); err != nil {
		t.Fatalf("run init returned error: %v", err)
	}

	for _, digit := range defaultDigits {
		data, err := os.ReadFile(filepath.Join(tempDir, "config-home", appName, "snippets", digit+".txt"))
		if err != nil {
			t.Fatalf("read initialized snippet: %v", err)
		}
		if string(data) != "from-local-"+digit {
			t.Fatalf("unexpected initialized content for %s: %q", digit, string(data))
		}
	}
}
