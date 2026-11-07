package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigWithSnippetAndCommandBindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"bindings":[
		{"key":"1","file":"snippets/1.txt"},
		{"hotkey":"Ctrl+Alt+Enter","command":["adb","-s","192.0.2.1:5555","shell","input","dpad","keycombination","-t","2000","23","23"]},
		{"hotkey":"Ctrl+Alt+2","command":["./tools/select.sh","argument with spaces"]}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bindings[0].File != filepath.Join(dir, "snippets", "1.txt") {
		t.Fatalf("snippet path changed: %#v", cfg.Bindings[0])
	}
	if cfg.Bindings[1].Command[0] != "adb" {
		t.Fatalf("PATH executable changed: %#v", cfg.Bindings[1])
	}
	if cfg.Bindings[2].Command[0] != filepath.Join(dir, "tools", "select.sh") || cfg.Bindings[2].Command[1] != "argument with spaces" {
		t.Fatalf("incorrect command path/arguments: %#v", cfg.Bindings[2])
	}
	compiled, err := compileBindings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled[1].Command, cfg.Bindings[1].Command) || compiled[1].Combo.trigger != 28 {
		t.Fatalf("command not carried into Enter binding: %#v", compiled[1])
	}
}

func TestLoadConfigRejectsAmbiguousOrInvalidCommands(t *testing.T) {
	cases := []string{
		`{"hotkey":"Ctrl+Alt+Enter"}`,
		`{"hotkey":"Ctrl+Alt+Enter","file":"snippet.txt","command":["adb"]}`,
		`{"hotkey":"Ctrl+Alt+Enter","command":[]}`,
		`{"hotkey":"Ctrl+Alt+Enter","command":[" "]}`,
		`{"hotkey":"Ctrl+Alt+Enter","command":["adb","bad\u0000argument"]}`,
		`{"hotkey":"Ctrl+Alt+Enter","command":"adb shell input keyevent 23"}`,
		`{"hotkey":"Ctrl+Alt+Enter","command":["adb"]},{"hotkey":"Ctrl+Alt+Return","file":"snippet.txt"}`,
	}
	for _, entry := range cases {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(`{"bindings":[`+entry+`]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path, dir); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

func TestCommandHotkeyWaitsForFullRelease(t *testing.T) {
	bindings, err := compileBindings(config{Bindings: []binding{{
		Hotkey: "Ctrl+Alt+Return", Command: []string{"adb", "shell", "input"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	w := hotkeyWatcher{bindings: bindings}
	pressed := map[int]int{}
	active, armed, last := make([]bool, 1), make([]bool, 1), make([]time.Time, 1)
	for _, event := range []struct{ key, value int }{
		{29, 1}, {56, 1}, {28, 1}, {28, -1}, {56, -1},
	} {
		updatePressedCountX11(pressed, x11Code(uint16(event.key)), event.value)
		if fired := w.evaluateX11(pressed, active, armed, last); len(fired) != 0 {
			t.Fatalf("command fired while a modifier was held: %#v", fired)
		}
	}
	updatePressedCountX11(pressed, x11Code(29), -1)
	fired := w.evaluateX11(pressed, active, armed, last)
	if len(fired) != 1 || !reflect.DeepEqual(fired[0].Command, bindings[0].Command) {
		t.Fatalf("expected command after full release: %#v", fired)
	}
	if fired := w.evaluateX11(pressed, active, armed, last); len(fired) != 0 {
		t.Fatal("command fired more than once")
	}
	mods, vk, err := parseWindowsHotkey(bindings[0].Hotkey)
	if err != nil || mods != windowsHotkeyModCtrl|windowsHotkeyModAlt || vk != 0x0D {
		t.Fatalf("Enter not supported by Windows backend: %d %d %v", mods, vk, err)
	}
}

func helperCommand(t *testing.T, args ...string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOTKEY_PASTE_TEST_PROCESS", "1")
	return append([]string{executable, "-test.run=^TestCommandBindingProcess$", "--"}, args...)
}

func TestCommandExecutesLiteralArgumentsAndReportsFailures(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	bad := runtimeBinding{Hotkey: "Ctrl+Alt+Enter", Command: helperCommand(t, "fail")}
	if err := executeBinding(context.Background(), bad, logger); err == nil || !strings.Contains(err.Error(), "device unavailable") {
		t.Fatalf("missing command failure output: %v", err)
	}
	out := filepath.Join(t.TempDir(), "arguments.json")
	want := []string{"two words", "$(touch should-not-exist)", "semi;colon", "", "--long-press"}
	good := runtimeBinding{Hotkey: "Ctrl+Alt+Enter", Command: helperCommand(t, append([]string{"write", out}, want...)...)}
	if err := executeBinding(context.Background(), good, logger); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command arguments changed: got %#v, want %#v", got, want)
	}
}

func TestCommandStopsWhenContextIsCanceled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	b := runtimeBinding{Command: helperCommand(t, "block", marker)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executeBinding(ctx, b, log.New(io.Discard, "", 0)) }()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("command stopped before starting: %v", err)
		case <-deadline:
			t.Fatal("command did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not stop after cancellation")
	}
}

func TestCommandBindingProcess(t *testing.T) {
	if os.Getenv("HOTKEY_PASTE_TEST_PROCESS") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	switch args[0] {
	case "fail":
		fmt.Fprintln(os.Stderr, "device unavailable")
		os.Exit(7)
	case "write":
		data, _ := json.Marshal(args[2:])
		if err := os.WriteFile(args[1], data, 0o600); err != nil {
			os.Exit(2)
		}
	case "block":
		if err := os.WriteFile(args[1], []byte("started"), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}
