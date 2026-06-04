package worker

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/mounts"
)

// HTTP handlers for the mounts API served directly from the worker. The
// control-plane proxies /api/sandboxes/:id/mounts to here in server mode (and
// in combined mode, the api.Server has its own handlers that hit the same
// underlying mounts.Service shape). Behavior identical to the CP-side handlers.

func (s *HTTPServer) addMount(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "mounts not available"})
	}
	id := c.Param("id")

	var req mounts.AddRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
	}

	var rec mounts.MountRecord
	routeOp := func(ctx context.Context) error {
		var err error
		rec, err = s.mountSvc.Add(ctx, id, req)
		return err
	}
	if err := s.routeOrCall(c, id, "mountAdd", routeOp); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mounts.ErrPersistenceUnavailable) {
			status = http.StatusServiceUnavailable
		}
		return c.JSON(status, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, rec)
}

func (s *HTTPServer) listMounts(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "mounts not available"})
	}
	return c.JSON(http.StatusOK, s.mountSvc.List(c.Param("id")))
}

func (s *HTTPServer) removeMount(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "mounts not available"})
	}
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}
	routeOp := func(ctx context.Context) error { return s.mountSvc.Remove(ctx, id, path) }
	if err := s.routeOrCall(c, id, "mountRemove", routeOp); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) routeOrCall(c echo.Context, sandboxID, opName string, op func(context.Context) error) error {
	if s.router != nil {
		return s.router.Route(c.Request().Context(), sandboxID, opName, op)
	}
	return op(c.Request().Context())
}
