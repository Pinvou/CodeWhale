package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminOverview struct {
	Users              int64 `json:"users"`
	ActiveUsers        int64 `json:"active_users"`
	DisabledUsers      int64 `json:"disabled_users"`
	Organizations      int64 `json:"organizations"`
	ActiveMemberships  int64 `json:"active_memberships"`
	PendingInvitations int64 `json:"pending_invitations"`
}

type AdminUser struct {
	ID                uuid.UUID  `json:"user_id"`
	DisplayName       *string    `json:"display_name,omitempty"`
	Email             *string    `json:"email,omitempty"`
	Status            string     `json:"status"`
	OrganizationCount int64      `json:"organization_count"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type AdminOrganization struct {
	ID          uuid.UUID `json:"organization_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	MemberCount int64     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AdminAuditLog struct {
	ID                   uuid.UUID       `json:"audit_id"`
	ActorUserID          *uuid.UUID      `json:"actor_user_id,omitempty"`
	ActiveOrganizationID *uuid.UUID      `json:"active_organization_id,omitempty"`
	Action               string          `json:"action"`
	ResourceType         string          `json:"resource_type"`
	ResourceID           string          `json:"resource_id"`
	Result               string          `json:"result"`
	RequestID            string          `json:"request_id"`
	ChangeSummary        json.RawMessage `json:"change_summary"`
	OccurredAt           time.Time       `json:"occurred_at"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminUsers(context.Context, string, int) ([]AdminUser, error)
	AdminSetUserStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteUser(context.Context, identity.Principal, uuid.UUID) error
	AdminOrganizations(context.Context, string, int) ([]AdminOrganization, error)
	AdminCreateOrganization(context.Context, identity.Principal, uuid.UUID, string) (Organization, error)
	AdminRenameOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminSetOrganizationStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteOrganization(context.Context, identity.Principal, uuid.UUID) error
	AdminUnits(context.Context, uuid.UUID) ([]Unit, error)
	AdminCreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string) (Unit, error)
	AdminRenameUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) error
	AdminDeleteUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	AdminAuditLogs(context.Context, int) ([]AdminAuditLog, error)
}

func (s *Service) requireAdminRepository(w http.ResponseWriter, r *http.Request) (adminRepository, bool) {
	repository, ok := s.repository.(adminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "account administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func adminLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	result, err := repository.AdminOverview(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminUsers(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminUsers(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), adminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminOrganizations(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminOrganizations(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), adminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string    `json:"name"`
		OwnerUserID uuid.UUID `json:"owner_user_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerUserID == uuid.Nil {
		if err == nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "organization name and owner are required", httpserver.RequestID(r.Context()))
		}
		return
	}
	organization, err := repository.AdminCreateOrganization(r.Context(), mustPrincipal(r.Context()), body.OwnerUserID, body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, organization)
}

func (s *Service) adminRenameOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := repository.AdminRenameOrganization(r.Context(), mustPrincipal(r.Context()), organizationID, body.Name); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminUnits(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	items, err := repository.AdminUnits(r.Context(), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminCreateUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name     string     `json:"name"`
		ParentID *uuid.UUID `json:"parent_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	unit, err := repository.AdminCreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, unit)
}

func (s *Service) adminRenameUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := repository.AdminRenameUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.Name); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	if err := repository.AdminDeleteUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetStatus(w, r, "user")
}

func (s *Service) adminSetOrganizationStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetStatus(w, r, "organization")
}

func (s *Service) adminSetStatus(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	pathName := resource + "_id"
	resourceID, ok := pathUUID(w, r, pathName)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.Status = strings.ToUpper(strings.TrimSpace(body.Status))
	if body.Status != "ACTIVE" && body.Status != "DISABLED" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "status must be ACTIVE or DISABLED", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	var err error
	if resource == "user" {
		err = repository.AdminSetUserStatus(r.Context(), principal, resourceID, body.Status)
	} else {
		err = repository.AdminSetOrganizationStatus(r.Context(), principal, resourceID, body.Status)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	s.adminDelete(w, r, "user")
}

func (s *Service) adminDeleteOrganization(w http.ResponseWriter, r *http.Request) {
	s.adminDelete(w, r, "organization")
}

func (s *Service) adminDelete(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	resourceID, ok := pathUUID(w, r, resource+"_id")
	if !ok {
		return
	}
	principal := mustPrincipal(r.Context())
	var err error
	if resource == "user" {
		err = repository.AdminDeleteUser(r.Context(), principal, resourceID)
	} else {
		err = repository.AdminDeleteOrganization(r.Context(), principal, resourceID)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminAuditLogs(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminAuditLogs(r.Context(), adminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM users WHERE status<>'DELETED'),
  (SELECT count(*) FROM users WHERE status='ACTIVE'),
  (SELECT count(*) FROM users WHERE status='DISABLED'),
  (SELECT count(*) FROM organizations WHERE status<>'DELETED'),
  (SELECT count(*) FROM memberships WHERE status='ACTIVE'),
  (SELECT count(*) FROM invitations WHERE status='PENDING' AND expires_at > now())`).Scan(
		&result.Users, &result.ActiveUsers, &result.DisabledUsers, &result.Organizations,
		&result.ActiveMemberships, &result.PendingInvitations,
	)
	return result, err
}

func (r *PostgresRepository) AdminUsers(ctx context.Context, search string, limit int) ([]AdminUser, error) {
	pattern := "%" + strings.ToLower(search) + "%"
	rows, err := r.pool.Query(ctx, `
SELECT u.id,u.display_name,u.email,u.status,
       (SELECT count(*) FROM memberships m WHERE m.user_id=u.id AND m.status='ACTIVE'),
       (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),
       u.created_at,u.updated_at
FROM users u
WHERE u.status<>'DELETED' AND ($1='' OR lower(COALESCE(u.display_name,'')) LIKE $2 OR lower(COALESCE(u.email,'')) LIKE $2 OR u.id::text=$1)
ORDER BY u.updated_at DESC,u.id LIMIT $3`, strings.ToLower(search), pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminUser, 0)
	for rows.Next() {
		var item AdminUser
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Email, &item.Status, &item.OrganizationCount, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminOrganizations(ctx context.Context, search string, limit int) ([]AdminOrganization, error) {
	pattern := "%" + strings.ToLower(search) + "%"
	rows, err := r.pool.Query(ctx, `
SELECT o.id,o.name,o.status,count(m.id) FILTER (WHERE m.status='ACTIVE'),o.created_at,o.updated_at
FROM organizations o LEFT JOIN memberships m ON m.organization_id=o.id
WHERE o.status<>'DELETED' AND ($1='' OR lower(o.name) LIKE $2 OR o.id::text=$1)
GROUP BY o.id ORDER BY o.updated_at DESC,o.id LIMIT $3`, strings.ToLower(search), pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminOrganization, 0)
	for rows.Next() {
		var item AdminOrganization
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &item.MemberCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminCreateOrganization(ctx context.Context, principal identity.Principal, ownerUserID uuid.UUID, name string) (Organization, error) {
	name = strings.TrimSpace(name)
	if ownerUserID == uuid.Nil || name == "" || len([]rune(name)) > 120 {
		return Organization{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Organization{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Organization{}, err
	}
	var ownerStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, ownerUserID).Scan(&ownerStatus)
	if errors.Is(err, pgx.ErrNoRows) || ownerStatus == "DELETED" {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if ownerStatus != "ACTIVE" {
		return Organization{}, ErrConflict
	}
	now := r.now()
	organization := Organization{ID: uuid.Must(uuid.NewV7()), Name: name, Status: "ACTIVE", Version: 1, Roles: []string{"OWNER"}}
	membershipID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,$2,'ACTIVE',$3,$4,$4)`, organization.ID, name, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, membershipID, organization.ID, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at) VALUES($1,$2,'OWNER',$3,$4)`, uuid.Must(uuid.NewV7()), membershipID, ownerUserID, now); err != nil {
		return Organization{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, ownerUserID, now); err != nil {
		return Organization{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organization.ID, "admin.organization.create", "organization", organization.ID.String(), map[string]any{"name": name, "owner_user_id": ownerUserID}); err != nil {
		return Organization{}, err
	}
	if err := appendOutbox(ctx, tx, "account.organization.created.v1", organization.ID, 1, map[string]any{"organization_id": organization.ID, "status": "ACTIVE"}); err != nil {
		return Organization{}, err
	}
	return organization, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRenameOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var currentName, status string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&currentName, &status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentName == name {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET name=$2,version=version+1,updated_at=$3 WHERE id=$1`, organizationID, name, r.now()); err != nil {
		return mapConstraintError(err)
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.rename", "organization", organizationID.String(), map[string]any{"from": currentName, "to": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminUnits(ctx context.Context, organizationID uuid.UUID) ([]Unit, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1 AND status<>'DELETED')`, organizationID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 ORDER BY created_at,id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Unit, 0)
	for rows.Next() {
		var item Unit
		if err := rows.Scan(&item.ID, &item.OrganizationID, &item.ParentID, &item.Name, &item.Status, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminCreateUnit(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name string) (Unit, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return Unit{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Unit{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return Unit{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return Unit{}, err
	}
	var organizationStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus)
	if errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return Unit{}, ErrNotFound
	}
	if err != nil {
		return Unit{}, err
	}
	if organizationStatus != "ACTIVE" {
		return Unit{}, ErrConflict
	}
	if parentID != nil {
		var parentActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND id=$2 AND status='ACTIVE')`, organizationID, *parentID).Scan(&parentActive); err != nil {
			return Unit{}, err
		}
		if !parentActive {
			return Unit{}, ErrInvalidHierarchy
		}
	}
	now := r.now()
	unit := Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_units(id,organization_id,parent_id,name,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ACTIVE',$5,$5)`, unit.ID, organizationID, parentID, name, now); err != nil {
		return Unit{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) VALUES($1,$2,$2,0)`, organizationID, unit.ID); err != nil {
		return Unit{}, err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) SELECT organization_id,ancestor_id,$3,depth+1 FROM organization_unit_closure WHERE organization_id=$1 AND descendant_id=$2`, organizationID, *parentID, unit.ID); err != nil {
			return Unit{}, err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.create", "organization_unit", unit.ID.String(), map[string]any{"name": name, "parent_id": parentID}); err != nil {
		return Unit{}, err
	}
	return unit, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRenameUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var currentName, unitStatus string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&currentName, &unitStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if unitStatus != "ACTIVE" {
		return ErrConflict
	}
	if currentName == name {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET name=$3,version=version+1,updated_at=$4 WHERE organization_id=$1 AND id=$2`, organizationID, unitID, name, r.now()); err != nil {
		return mapConstraintError(err)
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.rename", "organization_unit", unitID.String(), map[string]any{"from": currentName, "to": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var name, unitStatus string
	err = tx.QueryRow(ctx, `SELECT name,status FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&name, &unitStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if unitStatus != "ACTIVE" {
		return ErrConflict
	}
	var hasChildren bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND parent_id=$2)`, organizationID, unitID).Scan(&hasChildren); err != nil {
		return err
	}
	if hasChildren {
		return ErrInvalidHierarchy
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.user_id FROM memberships m LEFT JOIN membership_units mu ON mu.membership_id=m.id LEFT JOIN role_bindings rb ON rb.membership_id=m.id WHERE m.organization_id=$1 AND (mu.unit_id=$2 OR rb.unit_id=$2)`, organizationID, unitID)
	if err != nil {
		return err
	}
	affectedUsers := make([]uuid.UUID, 0)
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return err
		}
		affectedUsers = append(affectedUsers, userID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE unit_id=$1`, unitID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM membership_units WHERE unit_id=$1`, unitID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM organization_units WHERE organization_id=$1 AND id=$2`, organizationID, unitID); err != nil {
		return mapConstraintError(err)
	}
	if len(affectedUsers) > 0 {
		now := r.now()
		if _, err := tx.Exec(ctx, `UPDATE users SET membership_version=membership_version+1,updated_at=$2 WHERE id=ANY($1)`, affectedUsers, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET membership_version=membership_version+1,last_seen_at=$2 WHERE user_id=ANY($1)`, affectedUsers, now); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.delete", "organization_unit", unitID.String(), map[string]any{"name": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminAuditLogs(ctx context.Context, limit int) ([]AdminAuditLog, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,actor_user_id,active_organization_id,action,resource_type,resource_id,result,request_id,change_summary,occurred_at FROM audit_logs ORDER BY occurred_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminAuditLog, 0)
	for rows.Next() {
		var item AdminAuditLog
		if err := rows.Scan(&item.ID, &item.ActorUserID, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminSetUserStatus(ctx context.Context, principal identity.Principal, userID uuid.UUID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if actorID != nil && *actorID == userID {
		return ErrConflict
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) || currentStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == status {
		return tx.Commit(ctx)
	}
	if (status == "ACTIVE" && currentStatus != "DISABLED") || (status == "DISABLED" && currentStatus != "ACTIVE" && currentStatus != "PROVISIONING") {
		return ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE users SET status=$2,version=version+1,membership_version=membership_version+1,updated_at=$3 WHERE id=$1`, userID, status, now); err != nil {
		return err
	}
	if status == "DISABLED" {
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.user.status.update", "user", userID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteUser(ctx context.Context, principal identity.Principal, userID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if actorID != nil && *actorID == userID {
		return ErrConflict
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var ownsOrganization bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM memberships m
  JOIN role_bindings rb ON rb.membership_id=m.id AND rb.role='OWNER'
  JOIN organizations o ON o.id=m.organization_id AND o.status<>'DELETED'
  WHERE m.user_id=$1 AND m.status IN ('ACTIVE','SUSPENDED')
)`, userID).Scan(&ownsOrganization); err != nil {
		return err
	}
	if ownsOrganization {
		return ErrLastOwner
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2 WHERE user_id=$1 AND status<>'REVOKED'`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET email_normalized='deleted+'||left(id::text,8)||'@invalid.local',updated_at=$2 WHERE accepted_by=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status='DELETED',display_name=NULL,email=NULL,version=version+1,membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.user.delete", "user", userID.String(), map[string]any{"from": status, "to": "DELETED"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminSetOrganizationStatus(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) || currentStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == status {
		return tx.Commit(ctx)
	}
	if (status == "ACTIVE" && currentStatus != "DISABLED") || (status == "DISABLED" && currentStatus != "ACTIVE") {
		return ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE organizations SET status=$2,version=version+1,updated_at=$3 WHERE id=$1`, organizationID, status, now); err != nil {
		return err
	}
	fromMembershipStatus, toMembershipStatus := "SUSPENDED", "ACTIVE"
	if status == "DISABLED" {
		fromMembershipStatus, toMembershipStatus = "ACTIVE", "SUSPENDED"
	}
	if _, err := tx.Exec(ctx, `
WITH changed AS (
  UPDATE memberships SET status=$2,version=version+1,updated_at=$4
  WHERE organization_id=$1 AND status=$3 RETURNING user_id
)
UPDATE users SET membership_version=membership_version+1,updated_at=$4
WHERE id IN (SELECT user_id FROM changed)`, organizationID, toMembershipStatus, fromMembershipStatus, now); err != nil {
		return err
	}
	if status == "DISABLED" {
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type='PERSONAL',active_organization_id=NULL,membership_version=membership_version+1,last_seen_at=$2 WHERE active_organization_id=$1`, organizationID, now); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.status.update", "organization", organizationID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminDeleteOrganization(ctx context.Context, principal identity.Principal, organizationID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `
WITH changed AS (
  UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2
  WHERE organization_id=$1 AND status<>'REVOKED' RETURNING user_id
)
UPDATE users SET membership_version=membership_version+1,updated_at=$2
WHERE id IN (SELECT user_id FROM changed)`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET active_context_type='PERSONAL',active_organization_id=NULL,membership_version=membership_version+1,last_seen_at=$2 WHERE active_organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET status='DISABLED',name='Deleted unit '||left(id::text,8),version=version+1,updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status=CASE WHEN status='PENDING' THEN 'REVOKED' ELSE status END,email_normalized='deleted+'||left(id::text,8)||'@invalid.local',updated_at=$2 WHERE organization_id=$1`, organizationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET status='DELETED',name='Deleted organization '||left(id::text,8),version=version+1,updated_at=$2 WHERE id=$1`, organizationID, now); err != nil {
		return err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization.delete", "organization", organizationID.String(), map[string]any{"from": status, "to": "DELETED"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func adminActorID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (*uuid.UUID, error) {
	var actorID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &actorID, nil
}

func appendAdminAudit(ctx context.Context, tx pgx.Tx, actorID *uuid.UUID, principal identity.Principal, organizationID *uuid.UUID, action, resourceType, resourceID string, summary map[string]any) error {
	if summary == nil {
		summary = map[string]any{}
	}
	summary["actor_issuer"] = principal.Issuer
	summary["actor_subject"] = principal.Subject
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	requestID := httpserver.RequestID(ctx)
	if requestID == "" {
		requestID = "internal"
	}
	_, err = tx.Exec(ctx, `
INSERT INTO audit_logs(id,actor_user_id,session_id,active_organization_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,$7,'SUCCEEDED',$8,$8,$9,now())`, uuid.Must(uuid.NewV7()), actorID, principal.SessionID, organizationID, action, resourceType, resourceID, requestID, body)
	return err
}
