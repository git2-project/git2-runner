package server

import (
	"context"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
)

// secretServer implements civ1.SecretServiceServer. Values go in through
// SetSecret and never come out.
type secretServer struct {
	civ1.UnimplementedSecretServiceServer
	d Deps
}

func (s *secretServer) SetSecret(ctx context.Context, req *civ1.SetSecretRequest) (*civ1.SetSecretResponse, error) {
	scope := scopeFromProto(req.GetScope())
	scopeID, err := secrets.ValidateScope(scope, req.GetScopeId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	sec, created, err := s.d.Secrets.Set(ctx, scope, scopeID, req.GetName(), req.GetValue())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.SetSecretResponse{Secret: secretToProto(sec), Created: created}, nil
}

func (s *secretServer) ListSecrets(ctx context.Context, req *civ1.ListSecretsRequest) (*civ1.ListSecretsResponse, error) {
	scope := scopeFromProto(req.GetScope())
	scopeID, err := secrets.ValidateScope(scope, req.GetScopeId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	list, next, err := s.d.Secrets.List(ctx, scope, scopeID, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	resp := &civ1.ListSecretsResponse{NextPageToken: next}
	for _, sec := range list {
		resp.Secrets = append(resp.Secrets, secretToProto(sec))
	}
	return resp, nil
}

func (s *secretServer) DeleteSecret(ctx context.Context, req *civ1.DeleteSecretRequest) (*civ1.DeleteSecretResponse, error) {
	scope := scopeFromProto(req.GetScope())
	scopeID, err := secrets.ValidateScope(scope, req.GetScopeId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	if err := s.d.Secrets.Delete(ctx, scope, scopeID, req.GetName()); err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.DeleteSecretResponse{}, nil
}
