# hotkey-paste

`hotkey-paste` is a small background hotkey tool written in Go. On native Linux it behaves like before: start it from the CLI, it watches `Ctrl+Alt+1` through `Ctrl+Alt+0`, reads the text file associated with the hotkey, and injects that text into the currently focused application until you stop it with `Ctrl-C`. When you run the same Linux binary inside WSL, it can register the hotkeys with the Windows host and paste into the focused Windows application.

## Requirements

- Go 1.23 or newer to build
- One hotkey backend:
  - `xinput` on X11 sessions
  - `/dev/input/event*` fallback if `xinput` is unavailable
  - `powershell.exe` when running inside WSL and using the Windows host backend
- One typing backend:
  - `wtype` for Wayland-compatible typing
  - `xdotool` for X11 typing
  - `powershell.exe` for clipboard paste into Windows apps from WSL
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
- Inside WSL, the default auto-detection prefers a Windows-host backend through `powershell.exe`. That lets the Linux process register global Windows hotkeys and paste into the active Windows app without changing native Linux behavior.
- `HOTKEY_PASTE_HOTKEY_BACKEND` can be set to `xinput`, `evdev`, or `windows`.
- `HOTKEY_PASTE_OUTPUT_METHOD` can be set to `auto`, `windows`, `clipboard`, `wtype`, or `xdotool`.
- The WSL backend uses the Windows clipboard and sends `Ctrl+V` after the hotkey fires. It restores the previous text clipboard contents on a best-effort basis.

## Configuration

On first run the program creates:

- `~/.config/hotkey-paste/config.json`
- `~/.config/hotkey-paste/snippets/1.txt` through `~/.config/hotkey-paste/snippets/0.txt`

Missing snippet files are initialized from the project-local `./snippets` directory.

Edit `config.json` to change hotkeys or point them at different text files. Relative paths are resolved from `~/.config/hotkey-paste/`.

Example:

```json
{
  "bindings": [
    {
      "hotkey": "Ctrl+Alt+1",
      "file": "snippets/1.txt"
    },
    {
      "hotkey": "Ctrl+Shift+2",
      "file": "snippets/2.txt"
    }
  ]
}
```

Supported modifiers are `Ctrl`, `Alt`, `Shift`, and `Super`. If your desktop already uses one of the default `Ctrl+Alt+<digit>` shortcuts, change the conflicting `hotkey` entries in `config.json` to something unused.

`Ctrl+Alt+1` is preloaded with the requested report template. The other snippet files are created empty.
