// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Adb wraps the adb binary for one device.
type Adb struct {
	Path   string
	Serial string
}

// NewAdb creates an adb wrapper. serial may be empty for the default device.
func NewAdb(path, serial string) *Adb {
	if path == "" {
		path = "adb"
	}
	return &Adb{Path: path, Serial: serial}
}

func (a *Adb) baseArgs() []string {
	if a.Serial != "" {
		return []string{"-s", a.Serial}
	}
	return nil
}

func (a *Adb) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append(a.baseArgs(), args...)
	cmd := exec.CommandContext(ctx, a.Path, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("adb %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// Shell runs `adb shell <script>` and returns stdout as text.
// The script is passed as a single argument so the device-side shell parses it.
func (a *Adb) Shell(ctx context.Context, script string) (string, error) {
	out, err := a.run(ctx, "shell", script)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ExecOut runs `adb exec-out <script>` and returns raw bytes (no newline mangling).
func (a *Adb) ExecOut(ctx context.Context, script string) ([]byte, error) {
	return a.run(ctx, "exec-out", script)
}

// GetProp reads an Android system property.
func (a *Adb) GetProp(ctx context.Context, name string) (string, error) {
	out, err := a.Shell(ctx, "getprop "+ShellQuote(name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DeviceInfo gathers basic device identity.
func (a *Adb) DeviceInfo(ctx context.Context) (DeviceInfo, error) {
	info := DeviceInfo{Serial: a.Serial}
	model, err := a.GetProp(ctx, "ro.product.model")
	if err != nil {
		return info, err
	}
	info.Model = model
	info.AndroidRelease, _ = a.GetProp(ctx, "ro.build.version.release")
	sdk, _ := a.GetProp(ctx, "ro.build.version.sdk")
	info.AndroidSDK = atoi(sdk)
	if info.Serial == "" {
		// Best effort: first device listed.
		out, err := a.run(ctx, "get-serialno")
		if err == nil {
			info.Serial = strings.TrimSpace(string(out))
		}
	}
	return info, nil
}

// IsRoot reports whether the device grants root (su available and usable).
func (a *Adb) IsRoot(ctx context.Context) bool {
	out, err := a.Shell(ctx, "su -c id 2>/dev/null || echo NOROOT")
	if err != nil {
		return false
	}
	return strings.Contains(out, "uid=0")
}

// ShellQuote quotes a string for safe use inside a remote sh -c script.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>()*?[]#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
