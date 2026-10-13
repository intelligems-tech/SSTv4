// Package stepfunctions is the Step Functions pane of `sst dev`: each state
// machine the app deployed drawn as a graph, the recent visits to each
// state, and replays of a state or a whole execution.
package stepfunctions

import (
	"sort"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

// replaysKept bounds the replays the pane remembers.
const replaysKept = 100

type Machine struct {
	aws.StateMachineResource
	Definition *aws.Machine
	ParseError string
	Graph      *Graph
	// Executions are the recent executions, newest first.
	Executions []aws.StepFunctionExecution
	Error      string
	Loaded     bool
}

// Replay is a replay started from the pane and, once it ends, its result.
type Replay struct {
	aws.StepFunctionReplayStartedEvent
	Done   bool
	Result aws.StepFunctionReplayEvent
}

// Visit is one visit to a state with the execution it belongs to.
type Visit struct {
	aws.StepFunctionVisit
	Execution *aws.StepFunctionExecution
}

type Store struct {
	machines map[string]*Machine
	replays  []*Replay
}

func NewStore() *Store {
	return &Store{machines: map[string]*Machine{}}
}

// Machine returns a state machine by ARN.
func (s *Store) Machine(arn string) *Machine {
	return s.machines[arn]
}

// List lists the state machines by name.
func (s *Store) List() []*Machine {
	result := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		result = append(result, m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Apply folds one dev event in and reports whether the pane changed.
func (s *Store) Apply(evt interface{}) bool {
	switch evt := evt.(type) {
	case *project.CompleteEvent:
		if evt.Old || !evt.Finished {
			return false
		}
		s.deployed(aws.StateMachinesFromComplete(evt))
	case *aws.StepFunctionExecutionsEvent:
		m, ok := s.machines[evt.StateMachineArn]
		if !ok {
			return false
		}
		m.Executions, m.Error, m.Loaded = evt.Executions, evt.Error, true
	case *aws.StepFunctionReplayStartedEvent:
		s.replays = append(s.replays, &Replay{StepFunctionReplayStartedEvent: *evt})
		if len(s.replays) > replaysKept {
			s.replays = s.replays[1:]
		}
	case *aws.StepFunctionReplayEvent:
		r := s.replay(evt.ID)
		if r == nil {
			return evt.ID == ""
		}
		r.Done, r.Result = true, *evt
	default:
		return false
	}
	return true
}

// deployed replaces the machines with a deploy's; executions survive a
// redeploy, and an unchanged definition keeps its graph.
func (s *Store) deployed(resources []aws.StateMachineResource) {
	next := map[string]*Machine{}
	for _, resource := range resources {
		m, ok := s.machines[resource.Arn]
		if !ok {
			m = &Machine{}
		}
		if !ok || m.StateMachineResource.Definition != resource.Definition {
			m.Definition, m.Graph, m.ParseError = nil, nil, ""
			definition, err := aws.ParseDefinition(resource.Definition)
			if err != nil {
				m.ParseError = err.Error()
			} else {
				m.Definition, m.Graph = definition, NewGraph(definition)
			}
		}
		m.StateMachineResource = resource
		next[resource.Arn] = m
	}
	s.machines = next
}

func (s *Store) replay(id string) *Replay {
	for _, r := range s.replays {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Replays lists a state's replays (or a machine's execution replays when
// state is ""), newest first.
func (s *Store) Replays(arn string, state string) []*Replay {
	result := []*Replay{}
	for i := len(s.replays) - 1; i >= 0; i-- {
		r := s.replays[i]
		if r.StateMachineArn == arn && r.State == state {
			result = append(result, r)
		}
	}
	return result
}

// Visits lists the visits to a state across recent executions, newest
// first.
func (s *Store) Visits(arn string, state string) []Visit {
	m := s.machines[arn]
	if m == nil {
		return nil
	}
	result := []Visit{}
	for i := range m.Executions {
		execution := &m.Executions[i]
		for j := len(execution.Visits) - 1; j >= 0; j-- {
			if execution.Visits[j].State == state {
				result = append(result, Visit{StepFunctionVisit: execution.Visits[j], Execution: execution})
			}
		}
	}
	return result
}

// StateStatus sums up a state's visits in one execution: a failure wins,
// then a running visit, then a caught one. "" means not visited.
func StateStatus(execution *aws.StepFunctionExecution, state string) string {
	status := ""
	rank := map[string]int{"": 0, aws.VisitSucceeded: 1, aws.VisitCaught: 2, aws.VisitRunning: 3, aws.VisitFailed: 4}
	for _, visit := range execution.Visits {
		if visit.State == state && rank[visit.Status] > rank[status] {
			status = visit.Status
		}
	}
	return status
}

// Duration is how long a visit took, or has taken so far.
func (v Visit) Duration(now time.Time) time.Duration {
	if v.Exited.IsZero() {
		if v.Status == aws.VisitRunning {
			return now.Sub(v.Entered)
		}
		return 0
	}
	return v.Exited.Sub(v.Entered)
}
