package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
)

// fakeClient answers GetService with its states in order, repeating the last.
type fakeClient struct {
	andenv1.AndenClient
	states []andenv1.ServiceState
	err    error
}

func (f *fakeClient) GetService(context.Context, *andenv1.GetServiceRequest, ...grpc.CallOption) (*andenv1.Service, error) {
	if f.err != nil {
		return nil, f.err
	}
	state := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return &andenv1.Service{State: state}, nil
}

func TestSettleWaitsForTheState(t *testing.T) {
	client := &fakeClient{states: []andenv1.ServiceState{
		andenv1.ServiceState_SERVICE_STATE_SLEEPING, andenv1.ServiceState_SERVICE_STATE_ASLEEP,
	}}
	svc, err := settle(context.Background(), client, "s", andenv1.ServiceState_SERVICE_STATE_ASLEEP)
	if err != nil || svc.GetState() != andenv1.ServiceState_SERVICE_STATE_ASLEEP {
		t.Errorf("service %v, error %v", svc, err)
	}
}

func TestSettleReturnsTheLastStateWhenTimeIsUp(t *testing.T) {
	client := &fakeClient{states: []andenv1.ServiceState{andenv1.ServiceState_SERVICE_STATE_WAKING}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*settlePoll+settlePoll/2)
	defer cancel()
	start := time.Now()
	svc, err := settle(ctx, client, "s", andenv1.ServiceState_SERVICE_STATE_ACTIVE)
	if err != nil || svc.GetState() != andenv1.ServiceState_SERVICE_STATE_WAKING {
		t.Errorf("service %v, error %v", svc, err)
	}
	if time.Since(start) < settlePoll {
		t.Error("it did not wait")
	}
}

func TestSettleReturnsTheError(t *testing.T) {
	boom := errors.New("boom")
	client := &fakeClient{err: boom}
	if _, err := settle(context.Background(), client, "s", andenv1.ServiceState_SERVICE_STATE_ACTIVE); !errors.Is(err, boom) {
		t.Errorf("error %v", err)
	}
}
