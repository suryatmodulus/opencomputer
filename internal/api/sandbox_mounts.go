package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// MountRecord describes one active rclone-backed FUSE mount inside a sandbox.
// Creds are intentionally absent — they live only in the in-VM tmpfs config
// file written during addMount. The worker keeps no copy.
type MountRecord struct {
	Path     string `json:"path"`
	Remote   string `json:"remote"`
	Backend  string `json:"backend,omitempty"`
	ReadOnly bool   `json:"readOnly"`
}

// mountRegistry is a process-local map of active mounts per sandbox. v1
// intentionally doesn't persist mounts across hibernate or worker restarts;
// the hibernate path runs `fusermount3 -u -a` defensively before savevm
// regardless of what's in here, so a stale or empty registry is never a
// correctness problem — at worst, list() under-reports.
type mountRegistry struct {
	mu sync.Mutex
	m  map[string][]MountRecord
}

func newMountRegistry() *mountRegistry {
	return &mountRegistry{m: make(map[string][]MountRecord)}
}

func (r *mountRegistry) put(sandboxID string, rec MountRecord) {
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

func (r *mountRegistry) remove(sandboxID, path string) {
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

func (r *mountRegistry) get(sandboxID string) []MountRecord {
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

func (r *mountRegistry) clear(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, sandboxID)
}

type addMountRequest struct {
	Path         string            `json:"path"`         // absolute path inside the VM
	Remote       string            `json:"remote"`       // rclone remote spec, e.g. "s3:my-bucket/sub"
	Backend      string            `json:"backend"`      // one of s3, gcs, azureblob, sftp, webdav, dropbox
	Creds        map[string]string `json:"creds"`        // backend-specific config keys
	RcloneConfig string            `json:"rcloneConfig"` // raw rclone config; takes precedence over backend+creds
	ReadOnly     *bool             `json:"readOnly"`     // default true
	MountOptions []string          `json:"mountOptions"` // extra args appended to `rclone mount`
}

func (s *Server) addMount(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	id := c.Param("id")

	var req addMountRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
	}
	if req.Path == "" || req.Remote == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path and remote are required"})
	}
	if !strings.HasPrefix(req.Path, "/") {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path must be absolute"})
	}

	rcloneConf, err := renderRcloneConfig(req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	readOnly := true
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}

	confPath := mountConfPath(req.Path)
	target := req.Path

	routeOp := func(ctx context.Context) error {
		// Old-image guard: surface a clear error rather than letting the user
		// puzzle out a cryptic `exec: rclone: not found`.
		probe, perr := s.manager.Exec(ctx, id, types.ProcessConfig{
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

		if _, err := s.manager.Exec(ctx, id, types.ProcessConfig{
			Command: "sudo",
			Args:    []string{"sh", "-c", "mkdir -p /run/oc-agent/mounts && chmod 700 /run/oc-agent/mounts"},
			Timeout: 10,
		}); err != nil {
			return fmt.Errorf("prepare config dir: %w", err)
		}

		if err := s.manager.WriteFile(ctx, id, confPath, rcloneConf); err != nil {
			return fmt.Errorf("write rclone config: %w", err)
		}
		// WriteFile creates mode 0644; tighten so the sandbox user can't read creds.
		if _, err := s.manager.Exec(ctx, id, types.ProcessConfig{
			Command: "sudo",
			Args:    []string{"chmod", "600", confPath},
			Timeout: 5,
		}); err != nil {
			return fmt.Errorf("chmod config: %w", err)
		}

		if _, err := s.manager.Exec(ctx, id, types.ProcessConfig{
			Command: "sudo",
			Args:    []string{"sh", "-c", fmt.Sprintf("mkdir -p %q && chown sandbox:sandbox %q", target, target)},
			Timeout: 10,
		}); err != nil {
			return fmt.Errorf("prepare mount target: %w", err)
		}

		mountArgs := []string{
			"rclone", "mount", req.Remote, target,
			"--config", confPath,
			"--daemon",
			"--allow-other",
		}
		if readOnly {
			mountArgs = append(mountArgs, "--read-only")
		} else {
			mountArgs = append(mountArgs, "--vfs-cache-mode", "writes")
		}
		mountArgs = append(mountArgs, req.MountOptions...)

		resp, err := s.manager.Exec(ctx, id, types.ProcessConfig{
			Command: "sudo",
			Args:    mountArgs,
			Timeout: 30,
		})
		if err != nil {
			return fmt.Errorf("rclone mount: %w", err)
		}
		if resp.ExitCode != 0 {
			// Best-effort cleanup of the config file so creds don't linger after
			// a failed mount.
			_, _ = s.manager.Exec(ctx, id, types.ProcessConfig{
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

	if err := s.routeOrCall(c, id, "mountAdd", routeOp); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	rec := MountRecord{
		Path:     req.Path,
		Remote:   req.Remote,
		Backend:  req.Backend,
		ReadOnly: readOnly,
	}
	s.mounts.put(id, rec)
	return c.JSON(http.StatusCreated, rec)
}

func (s *Server) listMounts(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	id := c.Param("id")
	recs := s.mounts.get(id)
	if recs == nil {
		recs = []MountRecord{}
	}
	return c.JSON(http.StatusOK, recs)
}

func (s *Server) removeMount(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}
	confPath := mountConfPath(path)

	routeOp := func(ctx context.Context) error {
		_, err := s.manager.Exec(ctx, id, types.ProcessConfig{
			Command: "sudo",
			Args:    []string{"sh", "-c", fmt.Sprintf("fusermount3 -u %q 2>/dev/null; rm -f %q 2>/dev/null; true", path, confPath)},
			Timeout: 15,
		})
		return err
	}
	if err := s.routeOrCall(c, id, "mountRemove", routeOp); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	s.mounts.remove(id, path)
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) routeOrCall(c echo.Context, sandboxID, opName string, op func(context.Context) error) error {
	if s.router != nil {
		return s.router.Route(c.Request().Context(), sandboxID, opName, op)
	}
	return op(c.Request().Context())
}

// mountConfPath derives a deterministic tmpfs config-file path from the mount
// target. Tying the filename to the path means add→remove for the same path
// always touches the same config, and there's no need to track an opaque ID.
func mountConfPath(path string) string {
	sum := sha1.Sum([]byte(path))
	return "/run/oc-agent/mounts/" + hex.EncodeToString(sum[:])[:16] + ".conf"
}

// renderRcloneConfig builds a single-section rclone config from the typed
// backend+creds shape, or returns the raw user-supplied config if present.
//
// The section name is taken from the part before the colon in `remote`
// (rclone's "name:path" convention), so a remote like "s3:my-bucket" produces
// `[s3]` and `rclone mount s3:my-bucket /target` resolves correctly.
func renderRcloneConfig(req addMountRequest) (string, error) {
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
	// Stable key order so config rendering is deterministic (helps golden tests
	// and rules out spurious config-file churn on repeated adds).
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
