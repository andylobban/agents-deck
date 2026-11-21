// Package remotestatus reads versioned agent status snapshots over OpenSSH.
package remotestatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	defaultCommand = "~/apps/agent-status-helper/current/agent-status-helper.pyz"
	maxSnapshot    = 1 << 20
)

var destination = regexp.MustCompile(`^(?:[a-zA-Z0-9_][a-zA-Z0-9_.-]*@)?[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

type Config struct {
	Host    string `json:"host"`
	Command string `json:"command,omitempty"` // path on the remote host, without arguments
}

func (c Config) args() ([]string, error) {
	if !destination.MatchString(c.Host) {
		return nil, errors.New("host must be an SSH alias, hostname or user@host")
	}
	command := c.Command
	if command == "" {
		command = defaultCommand
	}
	if !(strings.HasPrefix(command, "/") || strings.HasPrefix(command, "~/")) || strings.ContainsFunc(command, unicode.IsControl) {
		return nil, errors.New("command must be an absolute path or a path starting with ~/")
	}
	home := ""
	if strings.HasPrefix(command, "~/") {
		home, command = `"$HOME"`, command[1:]
	}
	command = home + "'" + strings.ReplaceAll(command, "'", `'"'"'`) + "'"
	return []string{
		"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=4",
		"-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "PermitLocalCommand=no", "--", c.Host, command,
	}, nil
}

func (c Config) Validate() error {
	_, err := c.args()
	return err
}

type Snapshot struct {
	SchemaVersion int               `json:"schemaVersion"`
	Sources       map[string]Source `json:"sources"`
	Sessions      []Session         `json:"sessions"`
}

type Source struct {
	Status string   `json:"status"`
	Scopes []string `json:"scopes"`
}

type Session struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Scope     string    `json:"scope"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
}

// Authoritative reports whether a missing row can be removed immediately. A
// source absent from a successful snapshot is no longer configured.
func (s *Snapshot) Authoritative(source, scope string) bool {
	if s == nil {
		return false
	}
	state, exists := s.Sources[source]
	return !exists || state.Status == "ok" || state.Status == "partial" && slices.Contains(state.Scopes, scope)
}

func decode(payload []byte) (*Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil || len(snap.Sources) == 0 || snap.Sessions == nil {
		return nil, errors.New("invalid remote status snapshot")
	}
	if snap.SchemaVersion != 1 {
		return nil, errors.New("unsupported remote status schema version")
	}
	for _, source := range snap.Sources {
		switch source.Status {
		case "ok", "partial", "unavailable":
		default:
			return nil, errors.New("invalid remote source status")
		}
	}
	seen := make(map[[2]string]bool, len(snap.Sessions))
	for _, session := range snap.Sessions {
		key := [2]string{session.Source, session.ID}
		if session.ID == "" || session.Source == "" ||
			strings.ContainsRune(session.ID, 0) || strings.ContainsRune(session.Source, 0) || seen[key] {
			return nil, errors.New("missing or duplicate remote session identity")
		}
		if _, ok := snap.Sources[session.Source]; !ok || !snap.Authoritative(session.Source, session.Scope) {
			return nil, errors.New("remote session is outside a healthy source scope")
		}
		seen[key] = true
	}
	return &snap, nil
}

// OpenURL keeps browser links only and prevents the exec-based opener from
// treating an untrusted link as a command option or a local file.
func OpenURL(value string) string {
	u, err := url.ParseRequestURI(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(value, `\`) {
		return ""
	}
	return value
}

type boundedOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxSnapshot-b.buffer.Len() {
		b.overflow = true
		return 0, errors.New("remote status snapshot is too large")
	}
	return b.buffer.Write(p)
}

// Read runs only the configured snapshot executable on an existing SSH host.
func Read(ctx context.Context, config Config) (*Snapshot, error) {
	args, err := config.args()
	if err != nil {
		return nil, err
	}
	var out boundedOutput
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if out.overflow {
		return nil, errors.New("remote status snapshot is too large")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("OpenSSH client is unavailable")
	}
	var failure *exec.ExitError
	if err != nil && (!errors.As(err, &failure) || failure.ExitCode() != 2) {
		return nil, errors.New("SSH snapshot command failed; check the host key, login and command path")
	}
	// The helper exits 2 with a valid snapshot when all configured sources fail.
	snapshot, decodeErr := decode(out.buffer.Bytes())
	if decodeErr == nil && err != nil {
		for _, source := range snapshot.Sources {
			if source.Status != "unavailable" {
				return nil, errors.New("SSH snapshot command failed")
			}
		}
	}
	return snapshot, decodeErr
}
