package proxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- Clients -------------------------------------------------------------

// CreateClient inserts a new client and returns it.
func (s *Store) CreateClient(ctx context.Context, name, description string) (*Client, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("proxy: client name is required")
	}
	now := s.now()
	c := &Client{
		ID:          newID("client"),
		Name:        name,
		Description: strings.TrimSpace(description),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxy_clients(id, name, description, disabled, created_at, updated_at)
         VALUES(?, ?, ?, 0, ?, ?)`,
		c.ID, c.Name, c.Description, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return c, nil
}

// GetClient returns the client with the given id.
func (s *Store) GetClient(ctx context.Context, id string) (*Client, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, disabled, created_at, updated_at
         FROM proxy_clients WHERE id = ?`, id)
	return scanClient(row)
}

// ListClients returns all clients ordered by creation time.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, disabled, created_at, updated_at
         FROM proxy_clients ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetClientDisabled toggles the disabled flag for a client.
func (s *Store) SetClientDisabled(ctx context.Context, id string, disabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_clients SET disabled = ?, updated_at = ? WHERE id = ?`,
		disabled, s.now(), id)
	if err != nil {
		return fmt.Errorf("update client: %w", err)
	}
	return requireAffected(res)
}

// DeleteClient removes a client and (via ON DELETE CASCADE) its tokens, grants,
// and access requests.
func (s *Store) DeleteClient(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM proxy_clients WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	return requireAffected(res)
}

type clientScanner interface {
	Scan(dest ...any) error
}

func scanClient(sc clientScanner) (*Client, error) {
	var c Client
	err := sc.Scan(&c.ID, &c.Name, &c.Description, &c.Disabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan client: %w", err)
	}
	return &c, nil
}

// ---- Tokens --------------------------------------------------------------

// CreateToken generates a new bearer token for a client. The plaintext secret is
// returned once and never persisted; only its hash and a display prefix are
// stored. A ttl of 0 means the token never expires.
func (s *Store) CreateToken(ctx context.Context, clientID, note string, ttl time.Duration) (plaintext string, tok *Token, err error) {
	if _, err := s.GetClient(ctx, clientID); err != nil {
		return "", nil, err
	}
	plaintext, err = GenerateToken()
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	tok = &Token{
		ID:        newID("tok"),
		ClientID:  clientID,
		Prefix:    TokenDisplayPrefix(plaintext),
		Hash:      HashToken(plaintext),
		Note:      strings.TrimSpace(note),
		CreatedAt: now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		tok.ExpiresAt = &exp
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO proxy_tokens(id, client_id, token_hash, token_prefix, note, created_at, expires_at, revoked)
         VALUES(?, ?, ?, ?, ?, ?, ?, 0)`,
		tok.ID, tok.ClientID, tok.Hash, tok.Prefix, tok.Note, tok.CreatedAt, nullableTime(tok.ExpiresAt))
	if err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	return plaintext, tok, nil
}

// ListTokens returns a client's tokens (without secrets).
func (s *Store) ListTokens(ctx context.Context, clientID string) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, token_prefix, note, created_at, expires_at, last_used_at, revoked
         FROM proxy_tokens WHERE client_id = ? ORDER BY created_at ASC`, clientID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var expires, lastUsed sql.NullTime
		if err := rows.Scan(&t.ID, &t.ClientID, &t.Prefix, &t.Note, &t.CreatedAt, &expires, &lastUsed, &t.Revoked); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		if expires.Valid {
			t.ExpiresAt = &expires.Time
		}
		if lastUsed.Valid {
			t.LastUsedAt = &lastUsed.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks a token revoked so it can no longer authenticate.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE proxy_tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return requireAffected(res)
}

// AuthenticateToken resolves a plaintext bearer token to its client. It rejects
// unknown, revoked, or expired tokens and disabled clients. On success it
// updates the token's last_used_at timestamp.
func (s *Store) AuthenticateToken(ctx context.Context, plaintext string) (*Client, *Token, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return nil, nil, ErrNotFound
	}
	hash := HashToken(plaintext)
	row := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, token_prefix, note, created_at, expires_at, last_used_at, revoked
         FROM proxy_tokens WHERE token_hash = ?`, hash)
	var t Token
	var expires, lastUsed sql.NullTime
	err := row.Scan(&t.ID, &t.ClientID, &t.Prefix, &t.Note, &t.CreatedAt, &expires, &lastUsed, &t.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("authenticate token: %w", err)
	}
	if expires.Valid {
		t.ExpiresAt = &expires.Time
	}
	if lastUsed.Valid {
		t.LastUsedAt = &lastUsed.Time
	}
	if t.Revoked {
		return nil, nil, fmt.Errorf("proxy: token revoked: %w", ErrNotFound)
	}
	now := s.now()
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return nil, nil, fmt.Errorf("proxy: token expired: %w", ErrNotFound)
	}
	client, err := s.GetClient(ctx, t.ClientID)
	if err != nil {
		return nil, nil, err
	}
	if client.Disabled {
		return nil, nil, fmt.Errorf("proxy: client disabled: %w", ErrNotFound)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE proxy_tokens SET last_used_at = ? WHERE id = ?`, now, t.ID); err != nil {
		return nil, nil, fmt.Errorf("touch token: %w", err)
	}
	t.LastUsedAt = &now
	return client, &t, nil
}

// ---- Grants --------------------------------------------------------------

// AddGrant authorizes a client for a provider/model. It is idempotent: an
// existing identical grant is returned unchanged.
func (s *Store) AddGrant(ctx context.Context, clientID, provider, model, note string) (*Grant, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("proxy: grant requires provider and model")
	}
	if _, err := s.GetClient(ctx, clientID); err != nil {
		return nil, err
	}
	if existing, err := s.findGrant(ctx, clientID, provider, model); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	g := &Grant{
		ID:        newID("grant"),
		ClientID:  clientID,
		Provider:  provider,
		Model:     model,
		Note:      strings.TrimSpace(note),
		CreatedAt: s.now(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxy_grants(id, client_id, provider, model, note, created_at)
         VALUES(?, ?, ?, ?, ?, ?)`,
		g.ID, g.ClientID, g.Provider, g.Model, g.Note, g.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("add grant: %w", err)
	}
	return g, nil
}

// ListGrants returns a client's grants.
func (s *Store) ListGrants(ctx context.Context, clientID string) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, provider, model, note, created_at
         FROM proxy_grants WHERE client_id = ? ORDER BY provider, model`, clientID)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.ClientID, &g.Provider, &g.Model, &g.Note, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteGrant removes a grant by id.
func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM proxy_grants WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete grant: %w", err)
	}
	return requireAffected(res)
}

// HasGrant reports whether the client may call provider/model, honoring
// wildcard ("*") model grants.
func (s *Store) HasGrant(ctx context.Context, clientID, provider, model string) (bool, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM proxy_grants
         WHERE client_id = ? AND provider = ? AND (model = ? OR model = ?)`,
		clientID, provider, model, WildcardModel).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check grant: %w", err)
	}
	return n > 0, nil
}

func (s *Store) findGrant(ctx context.Context, clientID, provider, model string) (*Grant, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, provider, model, note, created_at
         FROM proxy_grants WHERE client_id = ? AND provider = ? AND model = ?`,
		clientID, provider, model)
	var g Grant
	err := row.Scan(&g.ID, &g.ClientID, &g.Provider, &g.Model, &g.Note, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find grant: %w", err)
	}
	return &g, nil
}

// ---- Access requests -----------------------------------------------------

// RequestAccess records a client's desire to use provider/model. Pending
// requests are deduplicated per (client, provider, model): a repeat request
// bumps the counter and updated_at rather than creating a duplicate row.
func (s *Store) RequestAccess(ctx context.Context, clientID, provider, model, reason string) (*AccessRequest, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if _, err := s.GetClient(ctx, clientID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.findPendingRequest(ctx, clientID, provider, model)
	if err == nil {
		now := s.now()
		if _, uerr := s.db.ExecContext(ctx,
			`UPDATE proxy_access_requests SET count = count + 1, updated_at = ?, reason = COALESCE(NULLIF(?, ''), reason)
             WHERE id = ?`,
			now, strings.TrimSpace(reason), existing.ID); uerr != nil {
			return nil, fmt.Errorf("bump access request: %w", uerr)
		}
		existing.Count++
		existing.UpdatedAt = now
		if r := strings.TrimSpace(reason); r != "" {
			existing.Reason = r
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// Cap distinct pending requests per client so a client cannot grow the DB
	// unbounded by probing many (provider, model) combinations. Repeat requests
	// for an existing (provider, model) dedupe above and never reach here.
	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM proxy_access_requests WHERE client_id = ? AND status = ?`,
		clientID, RequestPending).Scan(&pending); err != nil {
		return nil, fmt.Errorf("count pending access requests: %w", err)
	}
	if pending >= maxPendingRequestsPerClient {
		return nil, ErrTooManyPendingRequests
	}

	now := s.now()
	req := &AccessRequest{
		ID:        newID("req"),
		ClientID:  clientID,
		Provider:  provider,
		Model:     model,
		Status:    RequestPending,
		Reason:    strings.TrimSpace(reason),
		Count:     1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO proxy_access_requests(id, client_id, provider, model, status, reason, note, count, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, ?, '', 1, ?, ?)`,
		req.ID, req.ClientID, req.Provider, req.Model, req.Status, req.Reason, req.CreatedAt, req.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create access request: %w", err)
	}
	return req, nil
}

// ListAccessRequests returns access requests, optionally filtered by status
// (empty status returns all) and/or client.
func (s *Store) ListAccessRequests(ctx context.Context, status, clientID string) ([]AccessRequest, error) {
	q := `SELECT id, client_id, provider, model, status, reason, note, count, created_at, updated_at, decided_at
          FROM proxy_access_requests`
	var conds []string
	var args []any
	if strings.TrimSpace(status) != "" {
		conds = append(conds, "status = ?")
		args = append(args, strings.TrimSpace(status))
	}
	if strings.TrimSpace(clientID) != "" {
		conds = append(conds, "client_id = ?")
		args = append(args, strings.TrimSpace(clientID))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY updated_at DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list access requests: %w", err)
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		req, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// DecideAccessRequest approves or denies a pending request. Approving also adds
// the corresponding grant (unless the request's provider is empty, e.g. an
// unresolved/unknown model, in which case the caller must grant manually).
func (s *Store) DecideAccessRequest(ctx context.Context, id string, approve bool, note string) (*AccessRequest, error) {
	req, err := s.getAccessRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Status != RequestPending {
		return nil, fmt.Errorf("proxy: access request %q already %s", id, req.Status)
	}
	now := s.now()
	status := RequestDenied
	if approve {
		status = RequestApproved
		if strings.TrimSpace(req.Provider) != "" && strings.TrimSpace(req.Model) != "" {
			if _, gerr := s.AddGrant(ctx, req.ClientID, req.Provider, req.Model, "approved access request "+req.ID); gerr != nil {
				return nil, gerr
			}
		}
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE proxy_access_requests SET status = ?, note = ?, updated_at = ?, decided_at = ? WHERE id = ?`,
		status, strings.TrimSpace(note), now, now, id)
	if err != nil {
		return nil, fmt.Errorf("decide access request: %w", err)
	}
	req.Status = status
	req.Note = strings.TrimSpace(note)
	req.UpdatedAt = now
	req.DecidedAt = &now
	return req, nil
}

func (s *Store) getAccessRequest(ctx context.Context, id string) (*AccessRequest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, provider, model, status, reason, note, count, created_at, updated_at, decided_at
         FROM proxy_access_requests WHERE id = ?`, id)
	return scanAccessRequest(row)
}

func (s *Store) findPendingRequest(ctx context.Context, clientID, provider, model string) (*AccessRequest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, provider, model, status, reason, note, count, created_at, updated_at, decided_at
         FROM proxy_access_requests
         WHERE client_id = ? AND provider = ? AND model = ? AND status = ?`,
		clientID, provider, model, RequestPending)
	return scanAccessRequest(row)
}

func scanAccessRequest(sc clientScanner) (*AccessRequest, error) {
	var req AccessRequest
	var decided sql.NullTime
	err := sc.Scan(&req.ID, &req.ClientID, &req.Provider, &req.Model, &req.Status,
		&req.Reason, &req.Note, &req.Count, &req.CreatedAt, &req.UpdatedAt, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan access request: %w", err)
	}
	if decided.Valid {
		req.DecidedAt = &decided.Time
	}
	return &req, nil
}

// ---- Audit ---------------------------------------------------------------

// AppendAudit writes an audit entry. Failures are returned but callers on the
// request hot-path generally log-and-continue rather than failing the request.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("proxy: audit action is required")
	}
	if e.ID == "" {
		e.ID = newID("audit")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = s.now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxy_audit(id, client_id, provider, model, action, decision, detail, created_at)
         VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ClientID, e.Provider, e.Model, e.Action, e.Decision, e.Detail, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("append audit: %w", err)
	}
	return nil
}

// ListAudit returns recent audit entries (newest first), optionally filtered by
// client, up to limit rows (limit <= 0 uses a default of 200).
func (s *Store) ListAudit(ctx context.Context, clientID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, client_id, provider, model, action, decision, detail, created_at FROM proxy_audit`
	var args []any
	if strings.TrimSpace(clientID) != "" {
		q += " WHERE client_id = ?"
		args = append(args, strings.TrimSpace(clientID))
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ClientID, &e.Provider, &e.Model, &e.Action, &e.Decision, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Authorization -------------------------------------------------------

// Decision is the outcome of an authorization check.
type Decision struct {
	Allowed   bool
	Provider  string
	Model     string
	RequestID string // set when a pending access request was created/updated
	Reason    string
}

// Authorize checks whether a client may call provider/model. On success it
// records an allow audit entry. On failure it creates/dedupes a pending access
// request and records a deny audit entry, returning the request id so the caller
// can surface it in a structured 403.
func (s *Store) Authorize(ctx context.Context, clientID, provider, model string) (Decision, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)

	ok, err := s.HasGrant(ctx, clientID, provider, model)
	if err != nil {
		return Decision{}, err
	}
	if ok {
		_ = s.AppendAudit(ctx, AuditEntry{
			ClientID: clientID, Provider: provider, Model: model,
			Action: "authorize", Decision: "allow",
		})
		return Decision{Allowed: true, Provider: provider, Model: model}, nil
	}

	req, err := s.RequestAccess(ctx, clientID, provider, model, "auto: denied model call")
	if err != nil {
		if errors.Is(err, ErrTooManyPendingRequests) {
			// Still deny the call, but do not fail the request or grow the DB.
			_ = s.AppendAudit(ctx, AuditEntry{
				ClientID: clientID, Provider: provider, Model: model,
				Action: "authorize", Decision: "deny", Detail: "pending_request_cap_reached",
			})
			return Decision{
				Allowed:  false,
				Provider: provider,
				Model:    model,
				Reason:   "no grant for provider/model; pending access request limit reached",
			}, nil
		}
		return Decision{}, err
	}
	_ = s.AppendAudit(ctx, AuditEntry{
		ClientID: clientID, Provider: provider, Model: model,
		Action: "authorize", Decision: "deny", Detail: "access_request=" + req.ID,
	})
	return Decision{
		Allowed:   false,
		Provider:  provider,
		Model:     model,
		RequestID: req.ID,
		Reason:    "no grant for provider/model; a pending access request has been recorded",
	}, nil
}

func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
