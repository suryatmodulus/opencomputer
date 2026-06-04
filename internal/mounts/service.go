// Package mounts implements rclone-backed FUSE mounts inside a sandbox.
//
// Two callers wire this into HTTP layers:
//
//   - internal/api — combined-mode CP, where the same process hosts sandboxes
//   - internal/worker — the worker that owns the sandbox in server mode (the
//     CP proxies /api/sandboxes/:id/mounts here)
//
// Both call the same Service so behavior is identical regardless of which
// route the request took to reach the worker that actually owns the sandbox.
package mounts

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// MountRecord describes one rclone-backed FUSE mount inside a sandbox. For
// non-persistent mounts, the in-memory Registry is the only place it lives;
// for persistent mounts, a parallel row exists in the sandbox_mounts PG table.
//
// Status is one of:
//   - "active"     — mounted and serving I/O
//   - "replaying"  — wake-time remount in progress (persistent only)
//   - "failed"     — wake-time remount failed; Error is set (persistent only)
type MountRecord struct {
	Path       string `json:"path"`
	Remote     string `json:"remote"`
	Backend    string `json:"backend,omitempty"`
	ReadOnly   bool   `json:"readOnly"`
	Persistent bool   `json:"persistent,omitempty"`
	Status     string `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
}

// AddRequest is the wire shape the HTTP layer parses and hands to Service.Add.
type AddRequest struct {
	Path         string            `json:"path"`         // absolute path inside the VM
	Remote       string            `json:"remote"`       // rclone remote spec, e.g. "s3:my-bucket/sub"
	Backend      string            `json:"backend"`      // s3, gcs, azureblob, sftp, webdav, dropbox
	Creds        map[string]string `json:"creds"`        // backend-specific config keys
	RcloneConfig string            `json:"rcloneConfig"` // raw config; overrides backend+creds
	ReadOnly     *bool             `json:"readOnly"`     // default true
	Persistent   bool              `json:"persistent"`   // encrypt + persist in PG; auto-restore on wake
	MountOptions []string          `json:"mountOptions"` // extra args appended to `rclone mount`
}

// Service holds the cross-cutting state for the mounts feature.
//
// Registry is process-local and intentionally so: non-persistent mounts vanish
// on hibernate by design, and persistent mounts are rehydrated from PG on wake.
// Store is optional — when nil, persistent mounts are unavailable (Add returns
// a clear ErrPersistenceUnavailable).
type Service struct {
	manager  sandbox.Manager
	store    *db.Store
	registry *Registry
}

// ErrPersistenceUnavailable is returned by Service.Add when the caller passes
// Persistent:true but the server has no Store / no encryption key.
var ErrPersistenceUnavailable = fmt.Errorf("persistent mounts require OPENSANDBOX_SECRET_ENCRYPTION_KEY to be configured")

// NewService wires a Service. Pass nil for store when persistent mounts aren't
// supported in this binary (e.g. local dev without PG).
func NewService(manager sandbox.Manager, store *db.Store) *Service {
	return &Service{
		manager:  manager,
		store:    store,
		registry: newRegistry(),
	}
}

// List returns the current view of mounts the worker knows about for this
// sandbox. Non-nil empty slice when there are none, so HTTP handlers can
// marshal directly.
func (s *Service) List(sandboxID string) []MountRecord {
	recs := s.registry.get(sandboxID)
	if recs == nil {
		return []MountRecord{}
	}
	return recs
}

// Add validates the request, performs the live mount in the sandbox, records
// the in-memory entry, and (when Persistent) encrypts + persists the config so
// it survives hibernate.
//
// On rollback paths (persist failure after a successful live mount), Add tears
// down the live mount so the user view is consistent — no "persistent in name
// only" footgun.
func (s *Service) Add(ctx context.Context, sandboxID string, req AddRequest) (MountRecord, error) {
	if req.Path == "" || req.Remote == "" {
		return MountRecord{}, fmt.Errorf("path and remote are required")
	}
	if !strings.HasPrefix(req.Path, "/") {
		return MountRecord{}, fmt.Errorf("path must be absolute")
	}
	if req.Persistent {
		if s.store == nil || s.store.Encryptor() == nil {
			return MountRecord{}, ErrPersistenceUnavailable
		}
	}

	rcloneConf, err := renderRcloneConfig(req)
	if err != nil {
		return MountRecord{}, err
	}
	readOnly := true
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}

	if err := s.doMount(ctx, sandboxID, req.Path, rcloneConf, readOnly, req.MountOptions); err != nil {
		return MountRecord{}, err
	}

	rec := MountRecord{
		Path:       req.Path,
		Remote:     req.Remote,
		Backend:    req.Backend,
		ReadOnly:   readOnly,
		Persistent: req.Persistent,
		Status:     "active",
	}
	s.registry.put(sandboxID, rec)

	if req.Persistent {
		ct, err := s.store.Encryptor().Encrypt([]byte(rcloneConf))
		if err != nil {
			s.rollbackLiveMount(ctx, sandboxID, req.Path)
			return MountRecord{}, fmt.Errorf("encrypt mount config: %w", err)
		}
		if err := s.store.UpsertSandboxMount(ctx, db.PersistentMount{
			SandboxID:       sandboxID,
			Path:            req.Path,
			Remote:          req.Remote,
			Backend:         req.Backend,
			ReadOnly:        readOnly,
			MountOptions:    req.MountOptions,
			EncryptedConfig: ct,
		}); err != nil {
			s.rollbackLiveMount(ctx, sandboxID, req.Path)
			return MountRecord{}, fmt.Errorf("persist mount: %w", err)
		}
	}
	return rec, nil
}

// Remove unmounts and forgets the mount. Idempotent — no error when the path
// isn't mounted. Always best-effort PG delete (no error if the row never existed).
func (s *Service) Remove(ctx context.Context, sandboxID, path string) error {
	if err := s.unmountInVM(ctx, sandboxID, path); err != nil {
		return err
	}
	s.registry.remove(sandboxID, path)
	if s.store != nil {
		if err := s.store.DeleteSandboxMount(ctx, sandboxID, path); err != nil {
			log.Printf("mounts: failed to delete persistent mount row %s/%s: %v", sandboxID, path, err)
		}
	}
	return nil
}

// OnHibernate clears the non-persistent entries from the registry so list()
// reflects reality post-hibernate. Persistent entries stay (status flips to
// "replaying") so the UI shows the intent across the hibernate→wake gap.
//
// Call this from the same place that triggers `fusermount3 -u -a` in the VM
// (snapshot.go does the in-VM teardown; this is the registry-side complement).
func (s *Service) OnHibernate(sandboxID string) {
	s.registry.clearNonPersistent(sandboxID)
}

// OnWake is the SandboxRouter post-wake hook. Reads persistent mounts from
// PG, pre-populates the registry with status="replaying", and replays each
// mount in its own goroutine.
//
// Wake completion is NOT blocked on replay; failures surface as
// status="failed" in list() and last_error in PG. The user can remove the
// broken mount or re-add with fresh creds.
func (s *Service) OnWake(ctx context.Context, sandboxID string) {
	if s.store == nil || s.store.Encryptor() == nil {
		return
	}
	mounts, err := s.store.ListSandboxMounts(ctx, sandboxID)
	if err != nil {
		log.Printf("mounts: list persistent mounts for %s: %v", sandboxID, err)
		return
	}
	for _, m := range mounts {
		s.registry.put(sandboxID, MountRecord{
			Path:       m.Path,
			Remote:     m.Remote,
			Backend:    m.Backend,
			ReadOnly:   m.ReadOnly,
			Persistent: true,
			Status:     "replaying",
		})
		go s.replayOne(sandboxID, m)
	}
}

func (s *Service) replayOne(sandboxID string, m db.PersistentMount) {
	// Background context — wake replay must not be tied to whatever request
	// originally triggered the wake (it may have ended already).
	ctx := context.Background()
	plaintext, err := s.store.Encryptor().Decrypt(m.EncryptedConfig)
	if err != nil {
		s.markFailed(ctx, sandboxID, m, fmt.Sprintf("decrypt config: %v", err))
		return
	}
	if err := s.doMount(ctx, sandboxID, m.Path, string(plaintext), m.ReadOnly, m.MountOptions); err != nil {
		s.markFailed(ctx, sandboxID, m, err.Error())
		return
	}
	s.markActive(ctx, sandboxID, m)
}

func (s *Service) markActive(ctx context.Context, sandboxID string, m db.PersistentMount) {
	s.registry.put(sandboxID, MountRecord{
		Path: m.Path, Remote: m.Remote, Backend: m.Backend, ReadOnly: m.ReadOnly,
		Persistent: true, Status: "active",
	})
	if s.store != nil {
		_ = s.store.SetSandboxMountError(ctx, sandboxID, m.Path, "")
	}
}

func (s *Service) markFailed(ctx context.Context, sandboxID string, m db.PersistentMount, msg string) {
	log.Printf("mounts: persistent replay failed for %s/%s: %s", sandboxID, m.Path, msg)
	s.registry.put(sandboxID, MountRecord{
		Path: m.Path, Remote: m.Remote, Backend: m.Backend, ReadOnly: m.ReadOnly,
		Persistent: true, Status: "failed", Error: msg,
	})
	if s.store != nil {
		_ = s.store.SetSandboxMountError(ctx, sandboxID, m.Path, msg)
	}
}

func (s *Service) rollbackLiveMount(ctx context.Context, sandboxID, path string) {
	_ = s.unmountInVM(ctx, sandboxID, path)
	s.registry.remove(sandboxID, path)
}

// doMount is the shared orchestration: probe rclone is present, write the
// config to tmpfs, mkdir the target, exec `rclone mount --daemon`.
func (s *Service) doMount(ctx context.Context, sandboxID, target, rcloneConf string, readOnly bool, mountOptions []string) error {
	confPath := mountConfPath(target)

	probe, perr := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sh",
		Args:    []string{"-c", "command -v rclone >/dev/null 2>&1 && command -v fusermount3 >/dev/null 2>&1"},
		Timeout: 10,
	})
	if perr != nil {
		return fmt.Errorf("probe sandbox: %w", perr)
	}
	if probe == nil || probe.ExitCode != 0 {
		return fmt.Errorf("sandbox image is missing `rclone` and/or `fusermount3` — rebuild from the latest default rootfs to use mounts")
	}

	if _, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sudo",
		Args:    []string{"sh", "-c", "mkdir -p /run/oc-agent/mounts && chmod 700 /run/oc-agent/mounts"},
		Timeout: 10,
	}); err != nil {
		return fmt.Errorf("prepare config dir: %w", err)
	}

	if err := s.manager.WriteFile(ctx, sandboxID, confPath, rcloneConf); err != nil {
		return fmt.Errorf("write rclone config: %w", err)
	}
	if _, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sudo",
		Args:    []string{"chmod", "600", confPath},
		Timeout: 5,
	}); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}

	if _, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sudo",
		Args:    []string{"sh", "-c", fmt.Sprintf("mkdir -p %q && chown sandbox:sandbox %q", target, target)},
		Timeout: 10,
	}); err != nil {
		return fmt.Errorf("prepare mount target: %w", err)
	}

	remote := remoteFromConfig(rcloneConf)
	if remote == "" {
		return fmt.Errorf("could not derive remote name from rclone config")
	}

	mountArgs := []string{
		"rclone", "mount", remote, target,
		"--config", confPath,
		"--daemon",
		// --daemon waits for "mount ready" before forking; without a timeout, an
		// unreachable remote makes the call hang indefinitely (rclone's default
		// is "wait forever"). Cap at 30s so bad creds / network issues surface
		// as a clear error in the API response instead of an HTTP 524 from the
		// upstream proxy timeout.
		"--daemon-timeout", "30s",
		"--allow-other",
	}
	if readOnly {
		mountArgs = append(mountArgs, "--read-only")
	} else {
		mountArgs = append(mountArgs, "--vfs-cache-mode", "writes")
	}
	mountArgs = append(mountArgs, mountOptions...)

	resp, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sudo",
		Args:    mountArgs,
		Timeout: 30,
	})
	if err != nil {
		return fmt.Errorf("rclone mount: %w", err)
	}
	if resp.ExitCode != 0 {
		_, _ = s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
			Command: "sudo",
			Args:    []string{"rm", "-f", confPath},
			Timeout: 5,
		})
		msg := strings.TrimSpace(resp.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(resp.Stdout)
		}
		return fmt.Errorf("rclone mount failed (exit %d): %s", resp.ExitCode, msg)
	}
	return nil
}

func (s *Service) unmountInVM(ctx context.Context, sandboxID, path string) error {
	confPath := mountConfPath(path)
	_, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
		Command: "sudo",
		Args:    []string{"sh", "-c", fmt.Sprintf("fusermount3 -u %q 2>/dev/null; rm -f %q 2>/dev/null; true", path, confPath)},
		Timeout: 15,
	})
	return err
}

// --- Registry ---

type Registry struct {
	mu sync.Mutex
	m  map[string][]MountRecord
}

func newRegistry() *Registry {
	return &Registry{m: make(map[string][]MountRecord)}
}

func (r *Registry) put(sandboxID string, rec MountRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.m[sandboxID]
	for i := range cur {
		if cur[i].Path == rec.Path {
			cur[i] = rec
			r.m[sandboxID] = cur
			return
		}
	}
	r.m[sandboxID] = append(cur, rec)
}

func (r *Registry) remove(sandboxID, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.m[sandboxID]
	out := cur[:0]
	for _, rec := range cur {
		if rec.Path != path {
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		delete(r.m, sandboxID)
	} else {
		r.m[sandboxID] = out
	}
}

func (r *Registry) get(sandboxID string) []MountRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.m[sandboxID]
	if len(cur) == 0 {
		return nil
	}
	out := make([]MountRecord, len(cur))
	copy(out, cur)
	return out
}

func (r *Registry) clearNonPersistent(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.m[sandboxID]
	out := cur[:0]
	for _, rec := range cur {
		if rec.Persistent {
			rec.Status = "replaying"
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		delete(r.m, sandboxID)
	} else {
		r.m[sandboxID] = out
	}
}

// --- Helpers (exported because tests in other packages exercise them) ---

// MountConfPath derives a deterministic tmpfs path for the mount's rclone
// config. Hashing the target path means add→remove for the same mount path
// always touches the same file with no opaque ID to track.
func MountConfPath(path string) string { return mountConfPath(path) }

func mountConfPath(path string) string {
	sum := sha1.Sum([]byte(path))
	return "/run/oc-agent/mounts/" + hex.EncodeToString(sum[:])[:16] + ".conf"
}

// RemoteFromConfig extracts the first `[section]` name from a rclone config.
// rclone mount takes that name as the `<remote>:<path>` prefix on the command
// line, so we need it before invoking the mount.
func RemoteFromConfig(conf string) string { return remoteFromConfig(conf) }

func remoteFromConfig(conf string) string {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
		}
	}
	return ""
}

// RenderRcloneConfig builds a single-section rclone config from the typed
// backend+creds shape, or returns the raw user-supplied config if present.
// Stable key order so config rendering is deterministic.
func RenderRcloneConfig(req AddRequest) (string, error) { return renderRcloneConfig(req) }

func renderRcloneConfig(req AddRequest) (string, error) {
	if req.RcloneConfig != "" {
		return req.RcloneConfig, nil
	}
	colon := strings.Index(req.Remote, ":")
	if colon <= 0 {
		return "", fmt.Errorf(`remote must be "<name>:<path>" (got %q) when rcloneConfig is not supplied`, req.Remote)
	}
	name := req.Remote[:colon]

	var typ string
	switch req.Backend {
	case "s3":
		typ = "s3"
	case "gcs":
		typ = "google cloud storage"
	case "azureblob":
		typ = "azureblob"
	case "sftp":
		typ = "sftp"
	case "webdav":
		typ = "webdav"
	case "dropbox":
		typ = "dropbox"
	case "":
		return "", fmt.Errorf("backend is required when rcloneConfig is not supplied (or pass rcloneConfig directly)")
	default:
		return "", fmt.Errorf("unsupported backend %q (supported: s3, gcs, azureblob, sftp, webdav, dropbox — or pass rcloneConfig directly)", req.Backend)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\ntype = %s\n", name, typ)
	if req.Backend == "s3" {
		if _, ok := req.Creds["provider"]; !ok {
			b.WriteString("provider = AWS\n")
		}
	}
	keys := make([]string, 0, len(req.Creds))
	for k := range req.Creds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, req.Creds[k])
	}
	return b.String(), nil
}
