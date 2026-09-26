package ai

import (
	"context"
	"errors"
)

// Fake is a scripted provider for tests: each call returns the next
// response and records the request.
type Fake struct {
	Responses []*Response
	Requests  []Request
}

// Propose implements Provider.
func (f *Fake) Propose(_ context.Context, req Request) (*Response, error) {
	f.Requests = append(f.Requests, req)
	if len(f.Responses) == 0 {
		return nil, errors.New("fake provider: no scripted response left")
	}
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	return r, nil
}
