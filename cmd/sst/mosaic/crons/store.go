// Package crons is the Crons pane of `sst dev`: every scheduled Lambda the
// app deployed, its schedule and target, and runs started from the pane.
package crons

import (
	"sort"
	"strings"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

// perCron bounds how many runs a cron keeps.
const perCron = 20

type Status int

const (
	Running Status = iota
	Done
	Failed
)

// Execution is one manual run of a cron.
type Execution struct {
	ID        string
	RequestID string
	Payload   string
	Status    Status
	Error     string
	Output    string
	Duration  time.Duration
	At        time.Time
}

type Cron struct {
	aws.CronResource
	Deployed bool
	Runs     []*Execution
}

// Last is the most recent run, or nil.
func (c *Cron) Last() *Execution {
	if len(c.Runs) == 0 {
		return nil
	}
	return c.Runs[len(c.Runs)-1]
}

// Recent lists the cron's runs newest first.
func (c *Cron) Recent() []*Execution {
	result := make([]*Execution, 0, len(c.Runs))
	for i := len(c.Runs) - 1; i >= 0; i-- {
		result = append(result, c.Runs[i])
	}
	return result
}

type Store struct {
	crons map[string]*Cron
}

func NewStore() *Store {
	return &Store{crons: map[string]*Cron{}}
}

// Cron returns a cron by key.
func (s *Store) Cron(key string) *Cron {
	return s.crons[key]
}

// Apply folds one dev event in and reports whether the pane changed.
func (s *Store) Apply(evt interface{}) bool {
	switch evt := evt.(type) {
	case *project.CompleteEvent:
		if evt.Old || !evt.Finished {
			return false
		}
		s.deployed(aws.CronsFromComplete(evt))
	case *aws.CronRunStartedEvent:
		c, ok := s.crons[evt.Key]
		if !ok {
			return false
		}
		c.Runs = append(c.Runs, &Execution{ID: evt.RunID, Payload: evt.Payload, Status: Running, At: evt.At})
		if len(c.Runs) > perCron {
			c.Runs = c.Runs[1:]
		}
	case *aws.CronRunEvent:
		run := s.run(evt.Key, evt.RunID)
		if run == nil {
			return false
		}
		run.RequestID, run.Output, run.Duration = evt.RequestID, evt.Output, evt.Duration
		switch {
		case evt.Error != "":
			run.Status, run.Error = Failed, evt.Error
		case evt.FunctionError != "":
			run.Status, run.Error = Failed, functionError(evt.FunctionError, evt.Output)
		default:
			run.Status = Done
		}
	default:
		return false
	}
	return true
}

// deployed replaces the crons with a deploy's; runs survive a redeploy.
func (s *Store) deployed(resources []aws.CronResource) {
	next := map[string]*Cron{}
	for _, resource := range resources {
		c, ok := s.crons[resource.Key]
		if !ok {
			c = &Cron{}
		}
		c.CronResource, c.Deployed = resource, true
		next[resource.Key] = c
	}
	s.crons = next
}

func (s *Store) run(key string, id string) *Execution {
	c, ok := s.crons[key]
	if !ok {
		return nil
	}
	for _, run := range c.Runs {
		if run.ID == id {
			return run
		}
	}
	return nil
}

// List lists crons whose name contains filter, by name.
func (s *Store) List(filter string) []*Cron {
	filter = strings.ToLower(filter)
	result := []*Cron{}
	for _, c := range s.crons {
		if filter == "" || strings.Contains(strings.ToLower(c.Name), filter) {
			result = append(result, c)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// functionError is the error message a failed function returned, or the
// Lambda error type when the payload carries none.
func functionError(kind string, output string) string {
	if message := errorMessage(output); message != "" {
		return message
	}
	return kind
}
