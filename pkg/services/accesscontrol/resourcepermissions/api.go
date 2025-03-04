package resourcepermissions

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/org"
	"github.com/grafana/grafana/pkg/web"
)

type api struct {
	ac          accesscontrol.AccessControl
	router      routing.RouteRegister
	service     *Service
	permissions []string
}

func newApi(ac accesscontrol.AccessControl, router routing.RouteRegister, manager *Service) *api {
	permissions := make([]string, 0, len(manager.permissions))
	// reverse the permissions order for display
	for i := len(manager.permissions) - 1; i >= 0; i-- {
		permissions = append(permissions, manager.permissions[i])
	}
	return &api{ac, router, manager, permissions}
}

func (a *api) registerEndpoints() {
	auth := accesscontrol.Middleware(a.ac)
	licenseMW := a.service.options.LicenseMW
	if licenseMW == nil {
		licenseMW = nopMiddleware
	}

	a.router.Group(fmt.Sprintf("/api/access-control/%s", a.service.options.Resource), func(r routing.RouteRegister) {
		actionRead := fmt.Sprintf("%s.permissions:read", a.service.options.Resource)
		actionWrite := fmt.Sprintf("%s.permissions:write", a.service.options.Resource)
		scope := accesscontrol.Scope(a.service.options.Resource, a.service.options.ResourceAttribute, accesscontrol.Parameter(":resourceID"))
		r.Get("/description", auth(accesscontrol.EvalPermission(actionRead)), routing.Wrap(a.getDescription))
		r.Get("/:resourceID", auth(accesscontrol.EvalPermission(actionRead, scope)), routing.Wrap(a.getPermissions))
		r.Post("/:resourceID", licenseMW, auth(accesscontrol.EvalPermission(actionWrite, scope)), routing.Wrap(a.setPermissions))
		if a.service.options.Assignments.Users {
			r.Post("/:resourceID/users/:userID", licenseMW, auth(accesscontrol.EvalPermission(actionWrite, scope)), routing.Wrap(a.setUserPermission))
		}
		if a.service.options.Assignments.Teams {
			r.Post("/:resourceID/teams/:teamID", licenseMW, auth(accesscontrol.EvalPermission(actionWrite, scope)), routing.Wrap(a.setTeamPermission))
		}
		if a.service.options.Assignments.BuiltInRoles {
			r.Post("/:resourceID/builtInRoles/:builtInRole", licenseMW, auth(accesscontrol.EvalPermission(actionWrite, scope)), routing.Wrap(a.setBuiltinRolePermission))
		}
	})
}

type Assignments struct {
	Users           bool `json:"users"`
	ServiceAccounts bool `json:"serviceAccounts"`
	Teams           bool `json:"teams"`
	BuiltInRoles    bool `json:"builtInRoles"`
}

// swagger:response resourcePermissionsDescription
type DescriptionResponse struct {
	// in:body
	// required:true
	Body Description `json:"body"`
}

type Description struct {
	Assignments Assignments `json:"assignments"`
	Permissions []string    `json:"permissions"`
}

// swagger:route POST /access-control/:resource/description enterprise,access_control getResourceDescription
//
// Get a description of a resource's access control properties.
//
// Responses:
// 200: resourcePermissionsDescription
// 403: forbiddenError
// 500: internalServerError
func (a *api) getDescription(c *contextmodel.ReqContext) response.Response {
	return response.JSON(http.StatusOK, &Description{
		Permissions: a.permissions,
		Assignments: a.service.options.Assignments,
	})
}

type resourcePermissionDTO struct {
	ID               int64    `json:"id"`
	RoleName         string   `json:"roleName"`
	IsManaged        bool     `json:"isManaged"`
	IsInherited      bool     `json:"isInherited"`
	IsServiceAccount bool     `json:"isServiceAccount"`
	UserID           int64    `json:"userId,omitempty"`
	UserLogin        string   `json:"userLogin,omitempty"`
	UserAvatarUrl    string   `json:"userAvatarUrl,omitempty"`
	Team             string   `json:"team,omitempty"`
	TeamID           int64    `json:"teamId,omitempty"`
	TeamAvatarUrl    string   `json:"teamAvatarUrl,omitempty"`
	BuiltInRole      string   `json:"builtInRole,omitempty"`
	Actions          []string `json:"actions"`
	Permission       string   `json:"permission"`
}

// swagger:response getResourcePermissionsResponse
type getResourcePermissionsResponse []resourcePermissionDTO

// swagger:route POST /access-control/:resource/:resourceID enterprise,access_control getResourcePermissions
//
// Get permissions for a resource.
//
// Responses:
// 200: getResourcePermissionsResponse
// 403: forbiddenError
// 500: internalServerError
func (a *api) getPermissions(c *contextmodel.ReqContext) response.Response {
	resourceID := web.Params(c.Req)[":resourceID"]

	permissions, err := a.service.GetPermissions(c.Req.Context(), c.SignedInUser, resourceID)
	if err != nil {
		return response.Error(http.StatusInternalServerError, "failed to get permissions", err)
	}

	if a.service.options.Assignments.BuiltInRoles && !a.service.license.FeatureEnabled("accesscontrol.enforcement") {
		permissions = append(permissions, accesscontrol.ResourcePermission{
			Actions:     a.service.actions,
			Scope:       "*",
			BuiltInRole: string(org.RoleAdmin),
		})
	}

	dto := make(getResourcePermissionsResponse, 0, len(permissions))
	for _, p := range permissions {
		if permission := a.service.MapActions(p); permission != "" {
			teamAvatarUrl := ""
			if p.TeamId != 0 {
				teamAvatarUrl = dtos.GetGravatarUrlWithDefault(p.TeamEmail, p.Team)
			}

			dto = append(dto, resourcePermissionDTO{
				ID:               p.ID,
				RoleName:         p.RoleName,
				UserID:           p.UserId,
				UserLogin:        p.UserLogin,
				UserAvatarUrl:    dtos.GetGravatarUrl(p.UserEmail),
				Team:             p.Team,
				TeamID:           p.TeamId,
				TeamAvatarUrl:    teamAvatarUrl,
				BuiltInRole:      p.BuiltInRole,
				Actions:          p.Actions,
				Permission:       permission,
				IsManaged:        p.IsManaged,
				IsInherited:      p.IsInherited,
				IsServiceAccount: p.IsServiceAccount,
			})
		}
	}

	return response.JSON(http.StatusOK, dto)
}

type setPermissionCommand struct {
	Permission string `json:"permission"`
}

type setPermissionsCommand struct {
	Permissions []accesscontrol.SetResourcePermissionCommand `json:"permissions"`
}

// swagger:route POST /access-control/:resource/:resourceID/users/:userID enterprise,access_control setResourcePermissionsForUser
//
// Set resource permissions for a user.
//
// Assigns permissions for a resource by a given type (`:resource`) and `:resourceID` to a user or a service account.
// Allowed resources are `datasources`, `teams`, `dashboards`, `folders`, and `serviceaccounts`.
// Refer to the `/access-control/:resource/description` endpoint for allowed Permissions.
//
// Responses:
// 200: okRespoonse
// 400: badRequestError
// 403: forbiddenError
// 500: internalServerError
func (a *api) setUserPermission(c *contextmodel.ReqContext) response.Response {
	userID, err := strconv.ParseInt(web.Params(c.Req)[":userID"], 10, 64)
	if err != nil {
		return response.Error(http.StatusBadRequest, "userID is invalid", err)
	}
	resourceID := web.Params(c.Req)[":resourceID"]

	var cmd setPermissionCommand
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	_, err = a.service.SetUserPermission(c.Req.Context(), c.SignedInUser.GetOrgID(), accesscontrol.User{ID: userID}, resourceID, cmd.Permission)
	if err != nil {
		return response.Error(http.StatusBadRequest, "failed to set user permission", err)
	}

	return permissionSetResponse(cmd)
}

// swagger:route POST /access-control/:resource/:resourceID/teams/:teamID enterprise,access_control setResourcePermissionsForTeam
//
// Set resource permissions for a team.
//
// Assigns permissions for a resource by a given type (`:resource`) and `:resourceID` to a team.
// Allowed resources are `datasources`, `teams`, `dashboards`, `folders`, and `serviceaccounts`.
// Refer to the `/access-control/:resource/description` endpoint for allowed Permissions.
//
// Responses:
// 200: okRespoonse
// 400: badRequestError
// 403: forbiddenError
// 500: internalServerError
func (a *api) setTeamPermission(c *contextmodel.ReqContext) response.Response {
	teamID, err := strconv.ParseInt(web.Params(c.Req)[":teamID"], 10, 64)
	if err != nil {
		return response.Error(http.StatusBadRequest, "teamID is invalid", err)
	}
	resourceID := web.Params(c.Req)[":resourceID"]

	var cmd setPermissionCommand
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	_, err = a.service.SetTeamPermission(c.Req.Context(), c.SignedInUser.GetOrgID(), teamID, resourceID, cmd.Permission)
	if err != nil {
		return response.Error(http.StatusBadRequest, "failed to set team permission", err)
	}

	return permissionSetResponse(cmd)
}

// swagger:route POST /access-control/:resource/:resourceID/builtInRoles/:builtInRole enterprise,access_control setResourcePermissionsForBuiltInRole
//
// Set resource permissions for a built-in role.
//
// Assigns permissions for a resource by a given type (`:resource`) and `:resourceID` to a built-in role.
// Allowed resources are `datasources`, `teams`, `dashboards`, `folders`, and `serviceaccounts`.
// Refer to the `/access-control/:resource/description` endpoint for allowed Permissions.
//
// Responses:
// 200: okRespoonse
// 400: badRequestError
// 403: forbiddenError
// 500: internalServerError
func (a *api) setBuiltinRolePermission(c *contextmodel.ReqContext) response.Response {
	builtInRole := web.Params(c.Req)[":builtInRole"]
	resourceID := web.Params(c.Req)[":resourceID"]

	cmd := setPermissionCommand{}
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	_, err := a.service.SetBuiltInRolePermission(c.Req.Context(), c.SignedInUser.GetOrgID(), builtInRole, resourceID, cmd.Permission)
	if err != nil {
		return response.Error(http.StatusBadRequest, "failed to set role permission", err)
	}

	return permissionSetResponse(cmd)
}

// swagger:route POST /access-control/:resource/:resourceID enterprise,access_control setResourcePermissions
//
// Set resource permissions.
//
// Assigns permissions for a resource by a given type (`:resource`) and `:resourceID` to one or many
// assignment types. Allowed resources are `datasources`, `teams`, `dashboards`, `folders`, and `serviceaccounts`.
// Refer to the `/access-control/:resource/description` endpoint for allowed Permissions.
//
// Responses:
// 200: okRespoonse
// 400: badRequestError
// 403: forbiddenError
// 500: internalServerError
func (a *api) setPermissions(c *contextmodel.ReqContext) response.Response {
	resourceID := web.Params(c.Req)[":resourceID"]

	cmd := setPermissionsCommand{}
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	_, err := a.service.SetPermissions(c.Req.Context(), c.SignedInUser.GetOrgID(), resourceID, cmd.Permissions...)
	if err != nil {
		return response.Error(http.StatusBadRequest, "failed to set permissions", err)
	}

	return response.Success("Permissions updated")
}

func permissionSetResponse(cmd setPermissionCommand) response.Response {
	message := "Permission updated"
	if cmd.Permission == "" {
		message = "Permission removed"
	}
	return response.Success(message)
}
