package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const readScope = "runs:read"

type accessConfig struct {
	resource, issuer, introspection, clientID, clientSecret, owner string
}

type tokenClaims struct {
	Active   bool            `json:"active"`
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"`
	Scope    string          `json:"scope"`
	Type     string          `json:"token_type"`
	Expiry   int64           `json:"exp"`
}

func audienceMatches(raw json.RawMessage, expected string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil && one == expected {
		return true
	}
	var many []string
	return json.Unmarshal(raw, &many) == nil && slices.Contains(many, expected)
}

func (cfg accessConfig) verifyWith(client *http.Client) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token == "" || len(token) > 8192 {
			return nil, auth.ErrInvalidToken
		}
		form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.introspection, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		req.SetBasicAuth(url.QueryEscape(cfg.clientID), url.QueryEscape(cfg.clientSecret))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, auth.ErrInvalidToken
		}
		const maxClaims = 32 << 10
		data, err := io.ReadAll(io.LimitReader(res.Body, maxClaims+1))
		var claims tokenClaims
		if err != nil || len(data) > maxClaims || json.Unmarshal(data, &claims) != nil ||
			!claims.Active || !strings.EqualFold(claims.Type, "Bearer") ||
			claims.Issuer != cfg.issuer || claims.Subject != cfg.owner ||
			!audienceMatches(claims.Audience, cfg.resource) || claims.Expiry <= time.Now().Unix() {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Scopes: strings.Fields(claims.Scope),
			Expiration: time.Unix(claims.Expiry, 0), UserID: claims.Subject}, nil
	}
}
