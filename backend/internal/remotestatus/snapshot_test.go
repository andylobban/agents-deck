package remotestatus

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestArgumentsQuoteExecutableAndPinHostVerification(t *testing.T) {
	config := Config{Host: "agent@192.0.2.1", Command: "~/a directory/it's here"}
	args, err := config.args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--", "agent@192.0.2.1", `"$HOME"'/a directory/it'"'"'s here'`}
	if !slices.Equal(args[len(args)-3:], want) || !slices.Contains(args, "StrictHostKeyChecking=yes") || !slices.Contains(args, "BatchMode=yes") {
		t.Fatalf("unexpected SSH arguments: %q", args)
	}
	for _, bad := range []Config{{Host: "-oProxyCommand=x"}, {Host: "host", Command: "relative"}, {Host: "host", Command: "/bin/example\n"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted invalid config: %+v", bad)
		}
	}
}

func TestReadBoundsProcessOutputAndAcceptsUnavailableSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake OpenSSH uses sh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ssh := filepath.Join(dir, "ssh")
	write := func(script string) {
		t.Helper()
		if err := os.WriteFile(ssh, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := Config{Host: "192.0.2.1"}
	write(`echo '{"schemaVersion":1,"sources":{"t3":{"status":"unavailable"}},"sessions":[]}'
exit 2`)
	snapshot, err := Read(ctx, config)
	if err != nil || snapshot.Authoritative("t3", "project") {
		t.Fatalf("expected an unavailable source: %#v, %v", snapshot, err)
	}
	write("dd if=/dev/zero bs=65536 count=20 2>/dev/null")
	if _, err := Read(ctx, config); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized snapshot: %v", err)
	}
}

func TestDecodeAndBrowserLinks(t *testing.T) {
	for _, payload := range []string{
		`{"schemaVersion":2,"sources":{"t3":{"status":"ok"}},"sessions":[]}`,
		`{"schemaVersion":1,"sources":{"t3":{"status":"ok"}},"sessions":null}`,
		`{"schemaVersion":1,"sources":{"t3":{"status":"partial","scopes":["one"]}},"sessions":[{"id":"a","source":"t3","scope":"two"}]}`,
		`{"schemaVersion":1,"sources":{"t3":{"status":"ok"}},"sessions":[{"id":"a","source":"t3"},{"id":"a","source":"t3"}]}`,
	} {
		if _, err := decode([]byte(payload)); err == nil {
			t.Errorf("accepted invalid snapshot: %s", payload)
		}
	}
	for _, target := range []string{"file:///tmp/status", "https://auth@example.test/", "https:///chat", "[Thread: title]"} {
		if OpenURL(target) != "" {
			t.Errorf("accepted unsafe target %q", target)
		}
	}
	if target := "https://example.test/chat?resume=abc"; OpenURL(target) != target {
		t.Errorf("did not accept browser link %q", target)
	}
}
