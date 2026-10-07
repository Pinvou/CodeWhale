package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type AdminOwnerCandidate struct {
	ID                   uuid.UUID `json:"id"`
	Type                 string    `json:"type"`
	DisplayLabel         string    `json:"display_label"`
	Status               string    `json:"status"`
	AuthorizationVersion int64     `json:"-"`
	OwnerVersion         int64     `json:"-"`
	Normalized           string    `json:"-"`
}

type OwnerCandidateDirectory interface {
	Scan(context.Context, identity.Principal, string, *uuid.UUID, func([]AdminOwnerCandidate) error) error
}

type HTTPOwnerCandidateDirectory struct {
	endpoint     *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPOwnerCandidateDirectory(rawURL, serviceToken string) (*HTTPOwnerCandidateDirectory, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("ACCOUNT_HTTP_URL must be an absolute URL")
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/token-owner-candidates"
	return &HTTPOwnerCandidateDirectory{endpoint: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(AdminSnapshotBuildBudget)}, nil
}

func (d *HTTPOwnerCandidateDirectory) Scan(ctx context.Context, principal identity.Principal, ownerType string, organizationID *uuid.UUID, consume func([]AdminOwnerCandidate) error) error {
	cursor := ""
	authorityVersion := int64(0)
	for {
		endpoint := *d.endpoint
		query := endpoint.Query()
		query.Set("actor_issuer", principal.Issuer)
		query.Set("actor_subject", principal.Subject)
		query.Set("limit", "50000")
		if ownerType != "" {
			query.Set("owner_type", ownerType)
		}
		if organizationID != nil {
			query.Set("organization_id", organizationID.String())
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return err
		}
		if err = serviceauth.SetAuthorization(request, d.serviceToken); err != nil {
			return err
		}
		response, err := d.client.Do(request)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)
			response.Body.Close()
			if response.StatusCode == http.StatusForbidden {
				return ErrForbidden
			}
			if response.StatusCode == http.StatusGone {
				return ErrAdminPaginationCursorExpired
			}
			if response.StatusCode == http.StatusTooManyRequests {
				retryAfter, _ := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After")))
				if retryAfter < 1 || retryAfter > 60 {
					retryAfter = 1
				}
				return adminQueryRateLimitError{RetryAfter: retryAfter}
			}
			return errors.New("owner candidate authority unavailable")
		}
		var page struct {
			Items                []AdminOwnerCandidate `json:"items"`
			NextCursor           *string               `json:"next_cursor"`
			HasMore              bool                  `json:"has_more"`
			AuthorizationVersion int64                 `json:"authorization_version"`
			OwnerVersions        map[string]int64      `json:"owner_versions"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&page)
		response.Body.Close()
		if err != nil {
			return err
		}
		if page.AuthorizationVersion < 1 {
			return errors.New("owner candidate authority returned an invalid authorization version")
		}
		if authorityVersion == 0 {
			authorityVersion = page.AuthorizationVersion
		} else if authorityVersion != page.AuthorizationVersion {
			return ErrCatalogVersionConflict
		}
		for index := range page.Items {
			page.Items[index].AuthorizationVersion = page.AuthorizationVersion
			page.Items[index].OwnerVersion = page.OwnerVersions[page.Items[index].ID.String()]
			item := page.Items[index]
			if item.ID == uuid.Nil || item.OwnerVersion < 1 || !slices.Contains([]string{"USER", "ORGANIZATION", "UNIT"}, item.Type) || !slices.Contains([]string{"ACTIVE", "DISABLED", "PROVISIONING"}, item.Status) {
				return errors.New("owner candidate authority returned a non-selectable lifecycle")
			}
		}
		if err := consume(page.Items); err != nil {
			return err
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == nil || strings.TrimSpace(*page.NextCursor) == "" {
			return errors.New("owner candidate authority returned an invalid continuation")
		}
		cursor = *page.NextCursor
	}
	return nil
}

func normalizeOwnerCandidateLabel(value string) string {
	return cases.Fold().String(norm.NFKC.String(strings.TrimSpace(value)))
}

func (s *Service) ownerCandidateSnapshotSource(r *http.Request, filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	if s.ownerCandidates == nil {
		return nil, errors.New("owner candidate authority unavailable")
	}
	q, err := filter.string("q", 160)
	if err != nil {
		return nil, err
	}
	ownerType, err := filter.enum("owner_type", "USER", "ORGANIZATION", "UNIT")
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "ACTIVE", "DISABLED", "PROVISIONING")
	if err != nil {
		return nil, err
	}
	organizationID, err := filter.uuid("organization_id")
	if err != nil {
		return nil, err
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		return nil, ErrForbidden
	}
	normalizedQ := normalizeOwnerCandidateLabel(q)
	if normalizedQ != "" {
		filter.values["q"] = normalizedQ
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		filtered := make([]AdminOwnerCandidate, 0)
		overflow := false
		err := s.ownerCandidates.Scan(ctx, principal, ownerType, organizationID, func(items []AdminOwnerCandidate) error {
			for _, item := range items {
				item.DisplayLabel = norm.NFKC.String(strings.TrimSpace(item.DisplayLabel))
				item.Normalized = normalizeOwnerCandidateLabel(item.DisplayLabel)
				if status != "" && !strings.EqualFold(item.Status, status) {
					continue
				}
				if normalizedQ != "" && !strings.Contains(item.Normalized, normalizedQ) && item.ID.String() != normalizedQ {
					continue
				}
				if len(filtered) < maximum {
					filtered = append(filtered, item)
				} else {
					overflow = true
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if overflow {
			return ErrAdminPaginationFilterRequired
		}
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].Normalized != filtered[j].Normalized {
				return filtered[i].Normalized < filtered[j].Normalized
			}
			if filtered[i].Type != filtered[j].Type {
				return filtered[i].Type < filtered[j].Type
			}
			return filtered[i].ID.String() < filtered[j].ID.String()
		})
		rows := make([][]any, 0, len(filtered))
		for index, item := range filtered {
			encoded, _ := json.Marshal(item)
			rows = append(rows, []any{snapshotID, index, string(encoded)})
		}
		if len(rows) == 0 {
			return nil
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"token_admin_pagination_snapshot_members"}, []string{"snapshot_id", "position", "resource_id"}, pgx.CopyFromRows(rows))
		return err
	}, nil
}

func decodeOwnerCandidates(resourceIDs []string) ([]AdminOwnerCandidate, error) {
	items := make([]AdminOwnerCandidate, 0, len(resourceIDs))
	for _, encoded := range resourceIDs {
		var item AdminOwnerCandidate
		if json.Unmarshal([]byte(encoded), &item) != nil || item.ID == uuid.Nil || item.Type == "" {
			return nil, ErrAdminPaginationCursorInvalid
		}
		item.Normalized = ""
		items = append(items, item)
	}
	return items, nil
}
