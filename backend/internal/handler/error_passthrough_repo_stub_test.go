package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

// errorPassthroughRepoStub is a minimal service.ErrorPassthroughRepository for
// handler-level rule tests. Rules are returned as-is; the mutating methods are
// never exercised here.
type errorPassthroughRepoStub struct {
	rules []*model.ErrorPassthroughRule
}

func (s *errorPassthroughRepoStub) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return s.rules, nil
}

func (s *errorPassthroughRepoStub) GetByID(context.Context, int64) (*model.ErrorPassthroughRule, error) {
	return nil, nil
}

func (s *errorPassthroughRepoStub) Create(_ context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return rule, nil
}

func (s *errorPassthroughRepoStub) Update(_ context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return rule, nil
}

func (s *errorPassthroughRepoStub) Delete(context.Context, int64) error { return nil }
