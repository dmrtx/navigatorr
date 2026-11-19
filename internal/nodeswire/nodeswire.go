// Package nodeswire joins package nodes (the build registry) to the transcode
// engine without either importing the other: the engine sees the registry as
// action.BuildRecords, the registry sees the engine as nodes.Topology and
// nodes.TranscodeView. main (and tests) call Wire; nothing else does.
package nodeswire

import (
	"context"
	"sort"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/transcode"
)

// Topology is nodes.Topology over a worker set (nil reads as no nodes).
func Topology(set *transcode.WorkerSet) nodes.Topology { return topology{set} }

type topology struct{ set *transcode.WorkerSet }

func (t topology) Node(target string) (string, bool) {
	if t.set == nil {
		return "", false
	}
	w, ok := t.set.Get(target)
	if !ok {
		return "", false
	}
	return w.Name, true
}

func (t topology) Nodes() []string {
	if t.set == nil {
		return nil
	}
	var out []string
	for _, w := range t.set.All() {
		out = append(out, w.Name)
	}
	sort.Strings(out)
	return out
}

// TranscodeView is nodes.TranscodeView over the engine's stored pause state.
func TranscodeView(e *action.Engine) nodes.TranscodeView { return view{e} }

type view struct{ e *action.Engine }

func (v view) TranscodeStates(ctx context.Context) map[string]nodes.TranscodeState {
	out := map[string]nodes.TranscodeState{}
	if v.e == nil {
		return out
	}
	for name, st := range v.e.NodePauseStatesStored(ctx) {
		out[name] = nodes.TranscodeState{Paused: st.Paused, Mode: st.Mode, Since: st.Since, HeldBy: st.HeldBy, Message: st.Message}
	}
	return out
}

// Records is action.BuildRecords over a registry.
func Records(s *nodes.Service) action.BuildRecords { return records{s} }

type records struct{ s *nodes.Service }

func (r records) NodeBuildRecords(node string) []transcode.NodeBuild {
	var out []transcode.NodeBuild
	for _, b := range r.s.ActiveOn(node) {
		out = append(out, transcode.NodeBuild{
			ID: b.ID, Source: b.Source, Holder: b.Holder, Kind: b.Kind, Title: b.Title,
			PreemptMode: b.PreemptMode, Priority: b.Priority, Status: b.Status, StartedAt: b.StartedAt,
			Progress: b.Progress, CreatedAt: b.CreatedAt, LastHeartbeatAt: b.LastHeartbeatAt, ExpiresAt: b.ExpiresAt,
		})
	}
	return out
}

func (r records) BuildsPauses() (map[string]string, error) {
	m, err := r.s.BuildsPauses()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.Since
	}
	return out, nil
}

func (r records) SetBuildsPause(ctx context.Context, node string, paused bool) error {
	return r.s.SetBuildsPause(ctx, node, paused)
}

func (r records) BuildsPaused(node string) (bool, error) { return r.s.BuildsPaused(node) }

// Wire connects the engine to the registry and the registry to the engine.
// workers is the engine's worker set (nil when none are configured).
func Wire(e *action.Engine, s *nodes.Service) {
	if e != nil && s != nil {
		e.SetBuildRecords(Records(s))
	}
}
