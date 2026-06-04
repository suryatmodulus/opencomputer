package mounts

import (
	"strings"
	"testing"
)

func TestRenderRcloneConfig_RawPassthrough(t *testing.T) {
	raw := "[whatever]\ntype = s3\nfoo = bar\n"
	got, err := renderRcloneConfig(AddRequest{
		Remote:       "whatever:bucket",
		RcloneConfig: raw,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != raw {
		t.Errorf("raw passthrough mutated config: got %q want %q", got, raw)
	}
}

func TestRenderRcloneConfig_S3DefaultsAWSProvider(t *testing.T) {
	got, err := renderRcloneConfig(AddRequest{
		Remote:  "s3:my-bucket",
		Backend: "s3",
		Creds: map[string]string{
			"access_key_id":     "AKIA",
			"secret_access_key": "SECRET",
			"region":            "us-east-1",
		},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Section header derived from the remote name (before the colon).
	if !strings.HasPrefix(got, "[s3]\n") {
		t.Errorf("expected [s3] section header, got: %q", got)
	}
	// AWS default provider injected because Creds didn't set it.
	if !strings.Contains(got, "provider = AWS\n") {
		t.Errorf("expected provider=AWS default, got: %q", got)
	}
	// All three creds present.
	for _, want := range []string{
		"access_key_id = AKIA\n",
		"secret_access_key = SECRET\n",
		"region = us-east-1\n",
		"type = s3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in config:\n%s", want, got)
		}
	}
}

func TestRenderRcloneConfig_S3UserProviderWins(t *testing.T) {
	got, err := renderRcloneConfig(AddRequest{
		Remote:  "minio:bucket",
		Backend: "s3",
		Creds: map[string]string{
			"provider":          "Minio",
			"access_key_id":     "key",
			"secret_access_key": "secret",
			"endpoint":          "http://minio:9000",
		},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if strings.Contains(got, "provider = AWS\n") {
		t.Errorf("user-supplied provider was overridden; config:\n%s", got)
	}
	if !strings.Contains(got, "provider = Minio\n") {
		t.Errorf("expected provider=Minio, got:\n%s", got)
	}
}

func TestRenderRcloneConfig_GCSHasTypeWithSpace(t *testing.T) {
	got, err := renderRcloneConfig(AddRequest{
		Remote:  "gcs:my-bucket",
		Backend: "gcs",
		Creds:   map[string]string{"service_account_credentials": "{\"type\":\"service_account\"}"},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// rclone's type for GCS is literally "google cloud storage" (with spaces).
	if !strings.Contains(got, "type = google cloud storage\n") {
		t.Errorf("expected GCS type string, got:\n%s", got)
	}
}

func TestRenderRcloneConfig_DeterministicKeyOrder(t *testing.T) {
	creds := map[string]string{"zeta": "z", "alpha": "a", "mu": "m"}
	a, _ := renderRcloneConfig(AddRequest{Remote: "sftp:host", Backend: "sftp", Creds: creds})
	b, _ := renderRcloneConfig(AddRequest{Remote: "sftp:host", Backend: "sftp", Creds: creds})
	if a != b {
		t.Errorf("repeated render produced different output:\nA:\n%s\nB:\n%s", a, b)
	}
	// alpha must come before mu must come before zeta (sorted key order).
	idxA := strings.Index(a, "alpha = ")
	idxM := strings.Index(a, "mu = ")
	idxZ := strings.Index(a, "zeta = ")
	if !(idxA < idxM && idxM < idxZ) {
		t.Errorf("expected sorted key order alpha < mu < zeta, got:\n%s", a)
	}
}

func TestRenderRcloneConfig_RejectsBareRemote(t *testing.T) {
	_, err := renderRcloneConfig(AddRequest{Remote: "no-colon-here", Backend: "s3"})
	if err == nil {
		t.Fatal("expected error for remote without colon, got nil")
	}
}

func TestRenderRcloneConfig_RejectsUnknownBackend(t *testing.T) {
	_, err := renderRcloneConfig(AddRequest{Remote: "x:y", Backend: "minio-direct"})
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
	if !strings.Contains(err.Error(), "rcloneConfig") {
		t.Errorf("error should suggest rcloneConfig escape hatch, got: %v", err)
	}
}

func TestRenderRcloneConfig_RequiresBackendOrRawConfig(t *testing.T) {
	_, err := renderRcloneConfig(AddRequest{Remote: "x:y"})
	if err == nil {
		t.Fatal("expected error when no backend and no rcloneConfig, got nil")
	}
}

func TestMountConfPath_DeterministicAndSafe(t *testing.T) {
	a := mountConfPath("/mnt/data")
	b := mountConfPath("/mnt/data")
	if a != b {
		t.Errorf("mountConfPath not deterministic: %q vs %q", a, b)
	}
	c := mountConfPath("/mnt/other")
	if a == c {
		t.Errorf("different paths produced same conf path: %q", a)
	}
	if !strings.HasPrefix(a, "/run/oc-agent/mounts/") || !strings.HasSuffix(a, ".conf") {
		t.Errorf("conf path doesn't match expected shape: %q", a)
	}
	// The id segment must be filesystem-safe (no slashes, no path traversal).
	id := strings.TrimSuffix(strings.TrimPrefix(a, "/run/oc-agent/mounts/"), ".conf")
	if strings.ContainsAny(id, "/.\\ ") {
		t.Errorf("conf id contains unsafe chars: %q", id)
	}
}

func TestRemoteFromConfig(t *testing.T) {
	cases := []struct {
		name string
		conf string
		want string
	}{
		{"single section", "[s3]\ntype = s3\n", "s3"},
		{"leading whitespace", "  [gcs]  \ntype = google cloud storage\n", "gcs"},
		{"comment first then section", "# header comment\n[box]\ntype = box\n", "box"},
		{"empty config", "", ""},
		{"no section header", "type = s3\naccess_key_id = x\n", ""},
		{"first section wins", "[a]\ntype = s3\n[b]\ntype = sftp\n", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteFromConfig(tc.conf); got != tc.want {
				t.Errorf("remoteFromConfig(%q) = %q, want %q", tc.conf, got, tc.want)
			}
		})
	}
}

func TestMountRegistry_ClearNonPersistent(t *testing.T) {
	r := newRegistry()
	// Mix of persistent + non-persistent for the same sandbox.
	r.put("sb-1", MountRecord{Path: "/mnt/ephemeral", Remote: "s3:eph", Status: "active"})
	r.put("sb-1", MountRecord{Path: "/mnt/durable", Remote: "s3:dur", Persistent: true, Status: "active"})
	r.put("sb-1", MountRecord{Path: "/mnt/durable2", Remote: "s3:dur2", Persistent: true, Status: "active"})
	r.put("sb-2", MountRecord{Path: "/mnt/x", Remote: "s3:x", Status: "active"})

	r.clearNonPersistent("sb-1")

	got := r.get("sb-1")
	if len(got) != 2 {
		t.Fatalf("expected 2 persistent entries to remain on sb-1, got %d: %v", len(got), got)
	}
	for _, rec := range got {
		if !rec.Persistent {
			t.Errorf("non-persistent record leaked through clearNonPersistent: %+v", rec)
		}
		if rec.Status != "replaying" {
			t.Errorf("persistent record status after clearNonPersistent want %q got %q", "replaying", rec.Status)
		}
	}

	// Other sandboxes untouched.
	if got := r.get("sb-2"); len(got) != 1 {
		t.Errorf("clearNonPersistent leaked into other sandbox: %v", got)
	}
}

func TestMountRegistry_ClearNonPersistent_AllNonPersistent(t *testing.T) {
	r := newRegistry()
	r.put("sb-1", MountRecord{Path: "/mnt/a", Status: "active"})
	r.put("sb-1", MountRecord{Path: "/mnt/b", Status: "active"})
	r.clearNonPersistent("sb-1")
	if got := r.get("sb-1"); got != nil {
		t.Errorf("clearNonPersistent with no persistent entries should drop the sandbox; got %v", got)
	}
}

func TestMountRegistry_PutListRemove(t *testing.T) {
	r := newRegistry()
	r.put("sb-1", MountRecord{Path: "/mnt/a", Remote: "s3:a", Backend: "s3", ReadOnly: true})
	r.put("sb-1", MountRecord{Path: "/mnt/b", Remote: "s3:b", Backend: "s3", ReadOnly: false})
	r.put("sb-2", MountRecord{Path: "/mnt/x", Remote: "gcs:x", Backend: "gcs", ReadOnly: true})

	if got := r.get("sb-1"); len(got) != 2 {
		t.Errorf("sb-1 want 2 entries, got %d", len(got))
	}
	if got := r.get("sb-2"); len(got) != 1 {
		t.Errorf("sb-2 want 1 entry, got %d", len(got))
	}
	if got := r.get("sb-nope"); got != nil {
		t.Errorf("unknown sandbox should return nil, got %v", got)
	}

	// put with the same path updates in-place (not append).
	r.put("sb-1", MountRecord{Path: "/mnt/a", Remote: "s3:a-v2", Backend: "s3", ReadOnly: true})
	got := r.get("sb-1")
	if len(got) != 2 {
		t.Fatalf("re-put on same path should update, not append. Got %d entries: %v", len(got), got)
	}
	var found bool
	for _, rec := range got {
		if rec.Path == "/mnt/a" && rec.Remote == "s3:a-v2" {
			found = true
		}
	}
	if !found {
		t.Errorf("update didn't take effect: %v", got)
	}

	r.remove("sb-1", "/mnt/a")
	if got := r.get("sb-1"); len(got) != 1 || got[0].Path != "/mnt/b" {
		t.Errorf("after remove, expected [/mnt/b], got %v", got)
	}

	// Removing last entry should drop the sandbox key entirely (nil result).
	r.remove("sb-1", "/mnt/b")
	if got := r.get("sb-1"); got != nil {
		t.Errorf("after removing last entry, expected nil, got %v", got)
	}

	// clearNonPersistent() drops all non-persistent entries (in this test, all
	// of sb-2's entries are non-persistent).
	r.clearNonPersistent("sb-2")
	if got := r.get("sb-2"); got != nil {
		t.Errorf("after clearNonPersistent, expected nil, got %v", got)
	}
}
