# hotkey-paste

`hotkey-paste` is a background hotkey tool written in Go. Start it from the CLI to listen for configured shortcuts that paste text files into the focused application or run commands. It supports native Linux, WSL, and native Windows. Stop it with `Ctrl-C` in its terminal.

## Configured shortcuts

This table reflects `~/.config/hotkey-paste/config.json` and its snippet files as of November 7, 2026. These shortcuts are global while `hotkey-paste` is running. Press and release the shortcut once; the action runs after the trigger key and all modifiers are released.

| Shortcut in Ubuntu | Mac keys with default WorkSpaces mapping | Action |
| --- | --- | --- |
| `Ctrl+Alt+1` | `Command+Option+1` | Paste the full summary prompt from `snippets/1.txt`. |
| `Ctrl+Alt+2` | `Command+Option+2` | Paste the short-term equity news summary prompt from `snippets/2.txt`. |
| `Ctrl+Alt+3` | `Command+Option+3` | Paste the equity news signal and risk review prompt from `snippets/3.txt`. |
| `Ctrl+Alt+4` | `Command+Option+4` | Paste the brief summary prompt from `snippets/4.txt`. |
| `Ctrl+Alt+5` | `Command+Option+5` | Read `snippets/5.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+6` | `Command+Option+6` | Read `snippets/6.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+7` | `Command+Option+7` | Read `snippets/7.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+8` | `Command+Option+8` | Read `snippets/8.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+9` | `Command+Option+9` | Read `snippets/9.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+0` | `Command+Option+0` | Read `snippets/0.txt`; currently empty, so nothing is pasted. |
| `Ctrl+Alt+Shift+Y` | `Command+Option+Shift+Y` | Run `~/Documents/googletv/googletv.sh --long-press` to hold Google TV Select for two seconds. |

Snippet paths in this table are relative to `~/.config/hotkey-paste/`. The actual text is read from the file each time the shortcut fires. Edit a snippet to change its pasted text; restart `hotkey-paste` after changing the hotkey configuration. If `XDG_CONFIG_HOME` is set, the configuration directory is `$XDG_CONFIG_HOME/hotkey-paste/` instead.

For the Google TV shortcut, connect scrcpy and highlight the video using the arrow keys first. The shortcut sends the Select hold to the connected TV regardless of which desktop application has focus.

The [AWS WorkSpaces macOS client](https://docs.aws.amazon.com/workspaces/latest/userguide/amazon-workspaces-osx-client.html#osx_remap_command_key) maps Mac Command to remote Ctrl by default; Option corresponds to Alt. The table assumes this default mapping. The old `Ctrl+Alt+Enter` Google TV shortcut is no longer configured: its Mac equivalent, `Command+Option+Return`, is reserved by WorkSpaces for fullscreen.

## Requirements

- Go 1.23 or newer to build
- One hotkey backend:
  - `xinput` on X11 sessions
  - `/dev/input/event*` fallback if `xinput` is unavailable
  - `powershell.exe` on WSL or native Windows
- One typing backend:
  - `wtype` for Wayland-compatible typing
  - `xdotool` for X11 typing
  - `powershell.exe` for clipboard paste into Windows apps from WSL or native Windows
- Optional clipboard helpers for large snippets:
  - `wl-copy` and `wl-paste` on Wayland
  - `xclip` on X11

## Build

```bash
go build -o hotkey-paste .
```

## Usage

Initialize config and snippet files:

```bash
./hotkey-paste init
```

Overwrite the config snippets with the project-local `./snippets` files:

```bash
./hotkey-paste update
```

Run it:

```bash
./hotkey-paste
```

Or explicitly:

```bash
./hotkey-paste run
```

Stop it with `Ctrl-C`.

## Runtime Notes

- Hotkeys are no longer captured with an X11 key grab. The program now follows the reference project pattern and watches keyboard events through `xinput`, with `/dev/input/event*` as fallback.
- If the fallback backend is needed, your user may need read access to `/dev/input/event*` devices.
- Large snippets automatically use clipboard paste when the required clipboard tools are available. Smaller snippets use `wtype` or `xdotool`.
- Inside WSL, and on native Windows, the default auto-detection prefers the `powershell.exe` backend for global Windows hotkeys and paste.
- `HOTKEY_PASTE_HOTKEY_BACKEND` can be set to `xinput`, `evdev`, or `windows`.
- `HOTKEY_PASTE_OUTPUT_METHOD` can be set to `auto`, `windows`, `clipboard`, `wtype`, or `xdotool`.
- The Windows backend uses the Windows clipboard and sends `Ctrl+V` after the hotkey fires. It restores the previous text clipboard contents on a best-effort basis.

## Configuration

On first run the program creates:

- `~/.config/hotkey-paste/config.json`
- `~/.config/hotkey-paste/snippets/1.txt` through `~/.config/hotkey-paste/snippets/0.txt`

Missing snippet files are initialized from the project-local `./snippets` directory.
Run `./hotkey-paste update` to overwrite the config snippet files with the current project-local `./snippets` contents.

Edit `config.json` to change hotkeys, point them at different text files, or run a command. Relative file paths are resolved from `~/.config/hotkey-paste/`. Restart the program after editing the configuration.

Example:

```json
{
  "bindings": [
    {
      "hotkey": "Ctrl+Alt+1",
      "file": "snippets/1.txt"
    },
    {
      "hotkey": "Ctrl+Alt+2",
      "file": "snippets/2.txt"
    }
  ]
}
```

Supported modifiers are `Ctrl`, `Alt`, `Shift`, and `Super`. If your desktop already uses one of the default `Ctrl+Alt+<digit>` shortcuts, change the conflicting `hotkey` entries in `config.json` to something unused.

New configurations include the ten `Ctrl+Alt+<digit>` snippet bindings. Their initial text comes from the project-local `snippets` directory. The Google TV command binding is an additional configured shortcut, not part of those defaults.

## Command hotkeys

A binding can specify a `command` array instead of a `file`. Its first item is the executable; the remaining items are literal arguments. Commands run directly without shell expansion, and their output is not pasted. Executable names such as `adb` are found in `PATH`; executable paths containing a slash are resolved relative to the configuration directory, with `~/` also supported. Argument paths are passed unchanged.

For Google TV, append this binding to the existing `bindings` array:

```json
{
  "hotkey": "Ctrl+Alt+Shift+Y",
  "command": [
    "~/Documents/googletv/googletv.sh",
    "--long-press"
  ]
}
```

With scrcpy connected, highlight a video and press and release **Ctrl+Alt+Shift+Y** once. On a Mac with default WorkSpaces mapping, use **Command+Option+Shift+Y**. The script sends the two-second Select hold. This binding is already present in the configuration described above; do not add it twice.

Command hotkeys use the existing release handling: the trigger key and all modifiers must be released before the command runs. Actions run one at a time; commands are canceled when the program stops and time out after 30 seconds. A command failure is logged without stopping the hotkey watcher. Commands run on the computer hosting `hotkey-paste`, including when using WSL.

If you previously installed the separate GNOME shortcut for this action, remove it before using this binding, so one press does not run the action twice:

```bash
~/Documents/googletv/googletv.sh --remove-shortcut
```
