package desktop

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// editorHint tells the user how to fix the editor choice, on the right machine.
const editorHint = "on the desktop, export TWO_O_EDITOR='code --wait' (any editor that stays open until the file is closed) and restart the 2o daemon"

// guiEditors are graphical editors known to 2o, with the flag that makes
// them block until the file is closed. Order is detection preference.
var guiEditors = []struct{ name, block string }{
	{"code", "--wait"},
	{"codium", "--wait"},
	{"zed", "--wait"},
	{"gvim", "-f"},
	{"kate", "--block"},
	{"gedit", "--wait"},
	// Accepted from VISUAL/EDITOR, not auto-detected.
	{"code-insiders", "--wait"},
	{"subl", "--wait"},
	{"mvim", "-f"},
	{"emacsclient", "-c"},
	{"emacs", ""},
}

const detectedEditors = 6 // guiEditors[:detectedEditors] are probed on PATH

// terminalEditors need a terminal, which the desktop daemon does not have.
var terminalEditors = []string{"vi", "vim", "nvim", "nano", "pico", "micro", "helix", "hx", "kak", "joe", "ne", "mg", "ed", "vis"}

func guiBlockFlag(name string) (string, bool) {
	for _, e := range guiEditors {
		if e.name == name {
			return e.block, true
		}
	}
	return "", false
}

// needsTerminal reports whether an editor command line runs in a terminal.
func needsTerminal(fields []string) bool {
	name := filepath.Base(fields[0])
	if slices.Contains(terminalEditors, name) {
		return true
	}
	if name == "emacs" || name == "emacsclient" {
		for _, f := range fields[1:] {
			if f == "-nw" || f == "-t" || f == "--tty" || f == "--no-window-system" {
				return true
			}
		}
	}
	return false
}

// resolveEditor picks the editor command for 2o edit: TWO_O_EDITOR (taken
// as given, unless it is a terminal editor), then a detected graphical
// editor in its blocking mode, then VISUAL/EDITOR when they name a known
// graphical editor (the blocking flag is added when missing). Terminal
// editors are refused with a hint: the daemon has no terminal to show them.
func resolveEditor() ([]string, error) {
	var refused []string
	if v := strings.Fields(getenv("TWO_O_EDITOR")); len(v) > 0 {
		if needsTerminal(v) {
			return nil, unavailable("TWO_O_EDITOR=%q needs a terminal, which the desktop daemon does not have; %s", strings.Join(v, " "), editorHint)
		}
		return v, nil
	}
	for _, e := range guiEditors[:detectedEditors] {
		if has(e.name) {
			return []string{e.name, e.block}, nil
		}
	}
	for _, key := range []string{"VISUAL", "EDITOR"} {
		v := strings.Fields(getenv(key))
		if len(v) == 0 {
			continue
		}
		if needsTerminal(v) {
			refused = append(refused, fmt.Sprintf("%s=%s needs a terminal", key, v[0]))
			continue
		}
		flag, known := guiBlockFlag(filepath.Base(v[0]))
		if !known {
			refused = append(refused, fmt.Sprintf("%s=%s is not a known graphical editor", key, v[0]))
			continue
		}
		if !has(v[0]) {
			refused = append(refused, fmt.Sprintf("%s=%s is not installed", key, v[0]))
			continue
		}
		if flag != "" && !slices.Contains(v[1:], flag) {
			v = append(v, flag)
		}
		return v, nil
	}
	why := "no graphical editor found"
	if len(refused) > 0 {
		why += " (" + strings.Join(refused, "; ") + ")"
	}
	return nil, unavailable("%s; %s", why, editorHint)
}

// lineArgs returns the arguments opening path at line for known editors.
func lineArgs(editor, path string, line int) []string {
	if line <= 0 {
		return []string{path}
	}
	n := strconv.Itoa(line)
	switch filepath.Base(editor) {
	case "code", "code-insiders", "codium":
		return []string{"--goto", path + ":" + n}
	case "zed", "subl":
		return []string{path + ":" + n}
	case "kate":
		return []string{"--line", n, path}
	case "gvim", "mvim", "gedit", "emacs", "emacsclient":
		return []string{"+" + n, path}
	}
	return []string{path}
}

// Edit blocks until a graphical editor closes the file (line > 0 places the
// cursor there when the editor supports it). Errors say what failed and, for
// configuration problems, how to fix it; keeping the copy is the caller's call.
func Edit(path string, line int) error {
	editor, err := resolveEditor()
	if err != nil {
		return err
	}
	if !has(editor[0]) {
		return unavailable("editor %s is not installed; %s", editor[0], editorHint)
	}
	_, err = runTool(toolCall{name: editor[0], args: append(editor[1:len(editor):len(editor)], lineArgs(editor[0], path, line)...)})
	return err
}
