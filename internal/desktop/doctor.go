package desktop

import (
	"strings"
)

// CheckResult is one line of `2o doctor`.
type CheckResult struct {
	Name   string
	OK     bool
	Detail string
}

// Doctor reports what this desktop can do, without side effects: nothing is
// opened, copied or shown; only tool presence and read-only queries.
func Doctor() []CheckResult {
	s := FindSession()
	return []CheckResult{
		sessionCheck(s),
		openerCheck(),
		clipboardCheck(s),
		notifyCheck(),
		editorCheck(),
		pickerCheck(),
	}
}

func sessionCheck(s Session) CheckResult {
	r := CheckResult{Name: "graphical session"}
	switch {
	case goos == "darwin":
		r.OK, r.Detail = true, "macOS"
	case s.graphical():
		r.OK, r.Detail = true, s.describe()
	default:
		r.Detail = noSession().Error()
	}
	return r
}

func openerCheck() CheckResult {
	p, err := opener()
	if err != nil {
		return CheckResult{Name: "opener", Detail: strings.TrimPrefix(err.Error(), ErrUnavailable.Error()+": ")}
	}
	return CheckResult{Name: "opener", OK: true, Detail: p}
}

func clipboardCheck(s Session) CheckResult {
	r := CheckResult{Name: "clipboard"}
	if goos != "darwin" && !s.graphical() {
		r.Detail = "needs a graphical session"
		return r
	}
	var names []string
	for _, b := range usableBackends(s) {
		if b.copy == nil {
			names = append(names, b.name+" (read only)")
			continue
		}
		r.OK = true
		names = append(names, b.name)
	}
	if !r.OK {
		r.Detail = noToolHint
		if len(names) > 0 {
			r.Detail += "; found " + strings.Join(names, ", ")
		}
		return r
	}
	r.Detail = strings.Join(names, ", ")
	return r
}

// notifyCheck asks the notification daemon for its capabilities: without
// "actions", large-transfer confirmations cannot offer Accept/Refuse.
func notifyCheck() CheckResult {
	r := CheckResult{Name: "notifications"}
	if goos == "darwin" {
		r.OK, r.Detail = has("osascript"), "osascript"
		if !r.OK {
			r.Detail = "osascript is not installed"
		}
		return r
	}
	if !has("notify-send") {
		r.Detail = "notify-send is not installed (install libnotify)"
		return r
	}
	caps, err := notificationCapabilities()
	if err != nil {
		r.Detail = "no notification daemon answered: " + err.Error()
		return r
	}
	r.OK = true
	if caps["actions"] {
		r.Detail = "notification daemon supports Accept/Refuse buttons"
	} else {
		r.Detail = "notification daemon has no buttons: large transfers cannot be confirmed (set TWO_O_CONFIRM_BYTES=0 or TWO_O_MAX_BYTES)"
	}
	return r
}

// notificationCapabilities calls org.freedesktop.Notifications.GetCapabilities,
// which only reports the daemon's features.
func notificationCapabilities() (map[string]bool, error) {
	var call toolCall
	switch {
	case has("busctl"):
		call = toolCall{name: "busctl", args: []string{"--user", "call", "org.freedesktop.Notifications", "/org/freedesktop/Notifications", "org.freedesktop.Notifications", "GetCapabilities"}}
	case has("gdbus"):
		call = toolCall{name: "gdbus", args: []string{"call", "--session", "--dest", "org.freedesktop.Notifications", "--object-path", "/org/freedesktop/Notifications", "--method", "org.freedesktop.Notifications.GetCapabilities"}}
	default:
		return nil, unavailable("neither busctl nor gdbus is installed")
	}
	call.capture, call.limit, call.timeout = true, 64<<10, toolTimeout
	out, err := runTool(call)
	if err != nil {
		return nil, err
	}
	// busctl: as 2 "actions" "body"   gdbus: (['actions', 'body'],)
	caps := map[string]bool{}
	for _, f := range strings.FieldsFunc(string(out), func(r rune) bool { return strings.ContainsRune(" \t\n\"'[](),", r) }) {
		caps[f] = true
	}
	return caps, nil
}

func editorCheck() CheckResult {
	editor, err := resolveEditor()
	if err != nil {
		return CheckResult{Name: "editor", Detail: err.Error()}
	}
	if !has(editor[0]) {
		return CheckResult{Name: "editor", Detail: editor[0] + " is not installed; " + editorHint}
	}
	return CheckResult{Name: "editor", OK: true, Detail: strings.Join(editor, " ")}
}
