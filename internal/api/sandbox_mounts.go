package api

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/mounts"
)

// MountRecord and the request body type live in internal/mounts. Re-exposed
// here only so existing exported field names (used by SDKs) don't change.

func (s *Server) addMount(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
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
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, rec)
}

func (s *Server) listMounts(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	return c.JSON(http.StatusOK, s.mountSvc.List(c.Param("id")))
}

func (s *Server) removeMount(c echo.Context) error {
	if s.mountSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
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

func (s *Server) routeOrCall(c echo.Context, sandboxID, opName string, op func(context.Context) error) error {
	if s.router != nil {
		return s.router.Route(c.Request().Context(), sandboxID, opName, op)
	}
	return op(c.Request().Context())
}
