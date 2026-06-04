//go:build pgfixture

// Integration tests for the persistent-mount Store methods. Run only under
// `go test -tags=pgfixture` against a real Postgres pointed at by
// TEST_DATABASE_URL. The schema (UPSERT semantics, JSONB round-tripping,
// composite primary key) is what makes these worth running against actual PG
// rather than mocked.
//
// Run locally:
//
//	TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable \
//	  go test -tags=pgfixture ./internal/db/ -run Mount -v
package db

import (
	"bytes"
	"context"
	"reflect"
	"sort"
	"testing"
)

func TestSandboxMounts_UpsertListDelete_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	sb := "sb-mounts-test-1"
	t.Cleanup(func() { _ = store.DeleteSandboxMounts(ctx, sb) })

	m1 := PersistentMount{
		SandboxID:       sb,
		Path:            "/mnt/data",
		Remote:          "s3:my-bucket",
		Backend:         "s3",
		ReadOnly:        true,
		MountOptions:    []string{"--vfs-cache-mode", "writes"},
		EncryptedConfig: []byte{0x01, 0x02, 0x03},
	}
	m2 := PersistentMount{
		SandboxID:       sb,
		Path:            "/mnt/other",
		Remote:          "gcs:bucket-2",
		Backend:         "gcs",
		ReadOnly:        false,
		MountOptions:    nil,
		EncryptedConfig: []byte{0xff, 0xfe},
	}

	if err := store.UpsertSandboxMount(ctx, m1); err != nil {
		t.Fatalf("upsert m1: %v", err)
	}
	if err := store.UpsertSandboxMount(ctx, m2); err != nil {
		t.Fatalf("upsert m2: %v", err)
	}

	got, err := store.ListSandboxMounts(ctx, sb)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}
	// Order by path so the assertion is stable.
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })

	// JSONB round-trip: nil MountOptions on insert should come back as nil OR
	// empty slice — accept either, but both should be empty.
	if len(got[1].MountOptions) != 0 {
		t.Errorf("m2 expected empty MountOptions after round-trip, got %v", got[1].MountOptions)
	}
	if !reflect.DeepEqual(got[0].MountOptions, m1.MountOptions) {
		t.Errorf("m1 MountOptions round-trip: got %v want %v", got[0].MountOptions, m1.MountOptions)
	}
	if !bytes.Equal(got[0].EncryptedConfig, m1.EncryptedConfig) {
		t.Errorf("m1 encrypted blob round-trip differs")
	}
	if got[0].LastError != "" {
		t.Errorf("m1 LastError should be empty after upsert, got %q", got[0].LastError)
	}

	// UPSERT: re-insert m1 with different remote should overwrite, not duplicate.
	m1b := m1
	m1b.Remote = "s3:my-bucket-renamed"
	m1b.EncryptedConfig = []byte{0xaa, 0xbb}
	// Pre-poison last_error to confirm UPSERT clears it.
	if err := store.SetSandboxMountError(ctx, sb, m1.Path, "stale error"); err != nil {
		t.Fatalf("set error: %v", err)
	}
	if err := store.UpsertSandboxMount(ctx, m1b); err != nil {
		t.Fatalf("re-upsert m1: %v", err)
	}
	got, err = store.ListSandboxMounts(ctx, sb)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("UPSERT should not duplicate; got %d rows", len(got))
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })
	if got[0].Remote != "s3:my-bucket-renamed" {
		t.Errorf("re-upsert didn't update Remote: got %q", got[0].Remote)
	}
	if !bytes.Equal(got[0].EncryptedConfig, m1b.EncryptedConfig) {
		t.Errorf("re-upsert didn't update EncryptedConfig")
	}
	if got[0].LastError != "" {
		t.Errorf("UPSERT must clear last_error (incident-resilience); got %q", got[0].LastError)
	}

	// Single delete.
	if err := store.DeleteSandboxMount(ctx, sb, m1.Path); err != nil {
		t.Fatalf("delete m1: %v", err)
	}
	got, err = store.ListSandboxMounts(ctx, sb)
	if err != nil {
		t.Fatalf("list 3: %v", err)
	}
	if len(got) != 1 || got[0].Path != m2.Path {
		t.Errorf("after deleting m1, expected only m2; got %+v", got)
	}

	// Idempotent delete on missing row.
	if err := store.DeleteSandboxMount(ctx, sb, "/nope"); err != nil {
		t.Errorf("delete of missing row should be idempotent; got %v", err)
	}

	// Bulk delete.
	if err := store.DeleteSandboxMounts(ctx, sb); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	got, err = store.ListSandboxMounts(ctx, sb)
	if err != nil {
		t.Fatalf("list 4: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("bulk delete left rows: %+v", got)
	}
}

func TestSandboxMounts_SetSandboxMountError_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	sb := "sb-mounts-test-2"
	t.Cleanup(func() { _ = store.DeleteSandboxMounts(ctx, sb) })

	m := PersistentMount{
		SandboxID:       sb,
		Path:            "/mnt/data",
		Remote:          "s3:bucket",
		Backend:         "s3",
		ReadOnly:        true,
		EncryptedConfig: []byte{0x01},
	}
	if err := store.UpsertSandboxMount(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := store.SetSandboxMountError(ctx, sb, m.Path, "AccessDenied"); err != nil {
		t.Fatalf("set error: %v", err)
	}
	got, _ := store.ListSandboxMounts(ctx, sb)
	if len(got) != 1 || got[0].LastError != "AccessDenied" {
		t.Errorf("SetSandboxMountError didn't take effect: %+v", got)
	}

	// Clearing the error (empty string) is the post-successful-replay path.
	if err := store.SetSandboxMountError(ctx, sb, m.Path, ""); err != nil {
		t.Fatalf("clear error: %v", err)
	}
	got, _ = store.ListSandboxMounts(ctx, sb)
	if len(got) != 1 || got[0].LastError != "" {
		t.Errorf("clearing error didn't take effect: %+v", got)
	}

	// SetSandboxMountError on a missing (sandbox_id, path) is a no-op,
	// not an error — keeps the wake-time replay path lock-free.
	if err := store.SetSandboxMountError(ctx, sb, "/does-not-exist", "x"); err != nil {
		t.Errorf("SetSandboxMountError on missing row should not error; got %v", err)
	}
}

func TestSandboxMounts_ListEmptyReturnsEmptySlice_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	got, err := store.ListSandboxMounts(ctx, "sb-never-existed")
	if err != nil {
		t.Fatalf("list on unknown sandbox: %v", err)
	}
	if got == nil {
		t.Errorf("ListSandboxMounts should return non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %+v", got)
	}
}
