// Package vend creates and deletes ephemeral child tenants on a parent
// "vend" tenant. The proxy embeds this; cmd/vend used to be a thin CLI
// wrapper around the same calls.
package vend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/parsnips/ephemeral/auth"
)

// Vendor calls admin.createTenant / admin.deleteTenant on a parent tenant.
// One Vendor is bound to a single parent (the "vend" tenant) — it builds
// its own HTTP client with the right Authorization + X-Twisp-Account-Id
// headers from the supplied TokenSource.
type Vendor struct {
	httpClient *http.Client
	endpoint   string
	prefix     string
}

// Config configures a Vendor.
type Config struct {
	// Region for the endpoint URL (and inferred for STS by the caller).
	Region string
	// Env is the Twisp environment, e.g. "cloud" or "dev".
	Env string
	// VendAccountID is the parent tenant's accountId.
	VendAccountID string
	// Prefix is prepended to generated ephemeral accountIds. Defaults to "ephemeral".
	Prefix string
	// Source provides bearer tokens via sts:GetWebIdentityToken.
	Source *auth.TokenSource
	// Timeout bounds each GraphQL call. Defaults to 30s.
	Timeout time.Duration
}

// New returns a Vendor configured from cfg.
func New(cfg Config) (*Vendor, error) {
	if cfg.VendAccountID == "" {
		return nil, errors.New("vend: VendAccountID is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("vend: Source is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Env == "" {
		cfg.Env = "cloud"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "ephemeral"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Vendor{
		httpClient: &http.Client{
			Transport: auth.NewRoundTripper(cfg.Source, cfg.VendAccountID, nil),
			Timeout:   cfg.Timeout,
		},
		endpoint: fmt.Sprintf("https://api.%s.%s.twisp.com/financial/v1/graphql", cfg.Region, cfg.Env),
		prefix:   cfg.Prefix,
	}, nil
}

// Tenant is the result of Create.
type Tenant struct {
	ID        uuid.UUID
	AccountID string
}

// Create vends a fresh ephemeral tenant.
func (v *Vendor) Create(ctx context.Context) (Tenant, error) {
	id := uuid.New()
	accountID := fmt.Sprintf("%s-%s", v.prefix, strings.ReplaceAll(id.String(), "-", "")[:12])
	desc := fmt.Sprintf("ephemeral tenant vended at %s (prefix=%s)", time.Now().UTC().Format(time.RFC3339), v.prefix)
	if err := v.exec(ctx, createTenantMutation, map[string]any{
		"id":          id.String(),
		"accountId":   accountID,
		"name":        accountID,
		"description": desc,
	}); err != nil {
		return Tenant{}, fmt.Errorf("createTenant: %w", err)
	}
	return Tenant{ID: id, AccountID: accountID}, nil
}

// Delete tears down a tenant by accountId.
func (v *Vendor) Delete(ctx context.Context, accountID string) error {
	if err := v.exec(ctx, deleteTenantMutation, map[string]any{"accountId": accountID}); err != nil {
		return fmt.Errorf("deleteTenant: %w", err)
	}
	return nil
}

// BootstrapClient registers an auth client on a freshly-created tenant. Use
// when the new tenant doesn't inherit a client from its parent.
//
// httpClient must be configured to talk to the *target* tenant (i.e. its
// X-Twisp-Account-Id header must match accountID). policies is a JSON array
// matching the schema of CreateClientInput.policies.
func (v *Vendor) BootstrapClient(ctx context.Context, httpClient *http.Client, principal, name string, policies []byte) error {
	var parsed any
	if err := json.Unmarshal(policies, &parsed); err != nil {
		return fmt.Errorf("policies must be a JSON array: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"query": createClientMutation,
		"variables": map[string]any{
			"principal": principal,
			"name":      name,
			"policies":  parsed,
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readGraphQL(resp)
}

const createTenantMutation = `
mutation CreateTenant($id: UUID!, $accountId: String!, $name: String!, $description: String!) {
  admin {
    createTenant(input: { id: $id, accountId: $accountId, name: $name, description: $description }) {
      id
      accountId
    }
  }
}`

const deleteTenantMutation = `
mutation DeleteTenant($accountId: String!) {
  admin {
    deleteTenant(accountId: $accountId) { accountId }
  }
}`

const createClientMutation = `
mutation CreateClient($principal: String!, $name: String!, $policies: [PolicyInput]!) {
  auth {
    createClient(input: { principal: $principal, name: $name, policies: $policies }) {
      principal
    }
  }
}`

func (v *Vendor) exec(ctx context.Context, query string, vars map[string]any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readGraphQL(resp)
}

func readGraphQL(resp *http.Response) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("graphql %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode graphql: %w (body=%s)", err, string(raw))
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}
