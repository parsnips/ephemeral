// Package auth provides an http.RoundTripper and gRPC PerRPCCredentials that
// authenticate against Twisp using AWS IAM Outbound Identity Federation.
//
// The flow:
//
//  1. Call sts:GetWebIdentityToken to get a short-lived OIDC JWT issued by AWS.
//  2. Send that JWT as the Authorization bearer to Twisp.
//
// Twisp must have a Client whose `principal` matches the `iss` claim of the
// AWS-issued JWT (typically https://<account-uuid>.tokens.sts.global.api.aws).
//
// The same TokenSource can drive requests for any tenant in the org by varying
// the X-Twisp-Account-Id header — that is what makes this transport reusable
// for both the "vend" tenant and ephemeral tenants spawned from it.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/golang-jwt/jwt/v5"
)

const (
	HeaderAuthorization = "Authorization"
	HeaderAccountID     = "X-Twisp-Account-Id"

	// refreshLead is how long before expiry we refresh the token.
	refreshLead = 30 * time.Second
)

// TokenSource issues short-lived OIDC tokens from AWS STS.
type TokenSource struct {
	client    *sts.Client
	audience  string
	algorithm string

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewTokenSource builds a TokenSource backed by the given STS client.
//
// audience becomes the `aud` claim on the issued JWT. Pick something stable;
// Twisp policies can assert against it (e.g. context.auth.claims.aud == 'ephemeral').
func NewTokenSource(client *sts.Client, audience string) *TokenSource {
	return &TokenSource{
		client:    client,
		audience:  audience,
		algorithm: "RS256",
	}
}

// Token returns a cached token, refreshing if it is near expiry.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Until(s.expires) > refreshLead {
		return s.token, nil
	}

	out, err := s.client.GetWebIdentityToken(ctx, &sts.GetWebIdentityTokenInput{
		Audience:         []string{s.audience},
		SigningAlgorithm: aws.String(s.algorithm),
	})
	if err != nil {
		return "", fmt.Errorf("sts:GetWebIdentityToken: %w", err)
	}
	if out.WebIdentityToken == nil {
		return "", fmt.Errorf("sts:GetWebIdentityToken returned no token")
	}

	tok := *out.WebIdentityToken
	exp, err := parseExpiry(tok)
	if err != nil {
		return "", err
	}

	s.token = tok
	s.expires = exp
	return tok, nil
}

func parseExpiry(token string) (time.Time, error) {
	var claims jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse jwt: %w", err)
	}
	if claims.ExpiresAt == nil {
		// Default to a conservative 4 minute window if no exp claim.
		return time.Now().Add(4 * time.Minute), nil
	}
	return claims.ExpiresAt.Time, nil
}

// RoundTripper wraps an http.RoundTripper, attaching a Twisp Authorization
// bearer and X-Twisp-Account-Id header on each request.
type RoundTripper struct {
	Source    *TokenSource
	AccountID string
	Inner     http.RoundTripper
}

// NewRoundTripper returns an http.RoundTripper for the given tenant accountID.
func NewRoundTripper(source *TokenSource, accountID string, inner http.RoundTripper) *RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &RoundTripper{Source: source, AccountID: accountID, Inner: inner}
}

func (r *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := r.Source.Token(req.Context())
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(HeaderAuthorization, "Bearer "+tok)
	clone.Header.Set(HeaderAccountID, r.AccountID)
	return r.Inner.RoundTrip(clone)
}

// GRPCPerRPC is a credentials.PerRPCCredentials adapter, so the same TokenSource
// can authenticate gRPC calls.
type GRPCPerRPC struct {
	Source    *TokenSource
	AccountID string
}

func (g *GRPCPerRPC) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := g.Source.Token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"authorization":      "Bearer " + tok,
		"x-twisp-account-id": g.AccountID,
	}, nil
}

func (g *GRPCPerRPC) RequireTransportSecurity() bool { return true }
