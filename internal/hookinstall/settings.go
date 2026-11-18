package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// load reads a settings file into a generic JSON object. A missing or empty
// file is an empty object (raw is nil when the file does not exist). A file
// that is not a JSON object is refused rather than overwritten.
func load(path string) (root map[string]any, raw []byte, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("hookinstall: read settings: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep the user's numbers exactly as written
	if err := dec.Decode(&root); err != nil {
		return nil, nil, kerr.New(kerr.Invalid, "hookinstall: %s is not a JSON object: %v", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, kerr.New(kerr.Invalid, "hookinstall: %s has content after the JSON object", path)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, raw, nil
}

func encode(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("hookinstall: encode settings: %w", err)
	}
	return buf.Bytes(), nil
}

// writeAtomic replaces path through a temp file in the same directory, mode 0600.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("hookinstall: temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("hookinstall: temp file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("hookinstall: write settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("hookinstall: sync settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hookinstall: close settings: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("hookinstall: replace settings: %w", err)
	}
	return nil
}

// hooksObject returns root["hooks"], or nil when the key is absent.
func hooksObject(root map[string]any) (map[string]any, error) {
	raw, present := root["hooks"]
	if !present || raw == nil {
		return nil, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, kerr.New(kerr.Invalid, `hookinstall: "hooks" in the settings file is not an object`)
	}
	return hooks, nil
}

// apply writes the managed block into the settings object. Call it after removeOurs.
func apply(root map[string]any, s Settings) error {
	hooks, err := hooksObject(root)
	if err != nil {
		return err
	}
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	for _, h := range s.Hooks {
		groups, ok := hooks[h.Event].([]any)
		if !ok && hooks[h.Event] != nil {
			return kerr.New(kerr.Invalid, "hookinstall: hooks.%s in the settings file is not an array", h.Event)
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": h.Command, "timeout": h.Timeout}}}
		if h.Matcher != "" {
			group["matcher"] = h.Matcher
		}
		hooks[h.Event] = append(groups, group)
	}
	// The sandbox block is ours as a whole: a foreign key in it could only widen it.
	root["sandbox"] = map[string]any{
		"enabled": true, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
		"filesystem": map[string]any{"denyRead": list(s.Sandbox.DenyRead), "allowRead": list(s.Sandbox.AllowRead),
			"allowWrite": list(s.Sandbox.AllowWrite), "denyWrite": list(s.Sandbox.DenyWrite)},
	}
	env, ok := root["env"].(map[string]any)
	if !ok {
		if root["env"] != nil {
			return kerr.New(kerr.Invalid, `hookinstall: "env" in the settings file is not an object`)
		}
		env = map[string]any{}
	}
	for k, v := range s.Env {
		env[k] = v
	}
	if len(env) > 0 {
		root["env"] = env
	}
	perms, ok := root["permissions"].(map[string]any)
	if !ok {
		if root["permissions"] != nil {
			return kerr.New(kerr.Invalid, `hookinstall: "permissions" in the settings file is not an object`)
		}
		perms = map[string]any{}
		root["permissions"] = perms
	}
	old, ok := perms["deny"].([]any)
	if !ok && perms["deny"] != nil {
		return kerr.New(kerr.Invalid, `hookinstall: "permissions.deny" in the settings file is not an array`)
	}
	deny := []any{}
	for _, d := range old { // foreign rules stay; rules of the shapes we manage are replaced
		if rule, _ := d.(string); !managedDeny(rule) {
			deny = append(deny, d)
		}
	}
	perms["deny"] = append(deny, list(s.Deny)...)
	return nil
}

// managedDeny recognises the deny rules Generate writes, whatever paths they carry.
func managedDeny(rule string) bool {
	return strings.HasPrefix(rule, "Read(//") || strings.HasPrefix(rule, "Edit(//") ||
		rule == "WebFetch" || rule == "WebSearch" || rule == "mcp__*"
}

func list(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// removeOurs deletes our handlers and only the containers that this deletion emptied.
func removeOurs(root map[string]any, binary string) (changed bool, err error) {
	hooks, err := hooksObject(root)
	if err != nil || hooks == nil {
		return false, err
	}
	for _, e := range events {
		raw, present := hooks[e.Name]
		if !present || raw == nil {
			continue
		}
		groups, ok := raw.([]any)
		if !ok {
			return false, kerr.New(kerr.Invalid, "hookinstall: hooks.%s in the settings file is not an array", e.Name)
		}
		kept := make([]any, 0, len(groups))
		removed := false
		for _, g := range groups {
			group, _ := g.(map[string]any)
			handlers, _ := group["hooks"].([]any)
			rest := make([]any, 0, len(handlers))
			for _, h := range handlers {
				handler, _ := h.(map[string]any)
				if cmd, _ := handler["command"].(string); handler["type"] == "command" && isOurs(cmd, binary, e) {
					continue
				}
				rest = append(rest, h)
			}
			switch {
			case len(rest) == len(handlers): // nothing of ours in this group
				kept = append(kept, g)
			case len(rest) == 0: // the group held only our handler
				removed = true
			default:
				group["hooks"] = rest
				kept = append(kept, group)
				removed = true
			}
		}
		if !removed {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(hooks, e.Name)
		} else {
			hooks[e.Name] = kept
		}
	}
	if changed && len(hooks) == 0 {
		delete(root, "hooks")
	}
	return changed, nil
}
