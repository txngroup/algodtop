package tui

import (
	"context"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/model"
)

type fakeSource struct {
	nodes []api.NodeView
	st    api.DaemonStatus
}

func (f *fakeSource) Nodes(context.Context) ([]api.NodeView, api.DaemonStatus, error) {
	return f.nodes, f.st, nil
}
func (f *fakeSource) Events(context.Context, string, int64, int64, int) ([]model.Event, error) {
	return nil, nil
}
func (f *fakeSource) Series(context.Context, Query) (*Series, error) { return nil, ErrLive }
func (f *fakeSource) Templates(context.Context, string, int64, int64) ([]api.TemplateCount, error) {
	return nil, nil
}
func (f *fakeSource) PeerHistory(context.Context, string, int64, int64) ([]api.PeerHistory, error) {
	return nil, nil
}
func (f *fakeSource) Peers(context.Context, string) ([]collector.PeerInfo, error)   { return nil, nil }
func (f *fakeSource) AddAnnotation(context.Context, model.Annotation, string) error { return nil }
func (f *fakeSource) AddSilence(context.Context, []string, time.Duration, string) error {
	return nil
}
func (f *fakeSource) Live() bool       { return false }
func (f *fakeSource) Describe() string { return "fake" }
func (f *fakeSource) Close()           {}
