// Package invocations is the Invocations pane of `sst dev`: function
// invocations grouped by handler, each with its input, output, logs and
// error, browsable and searchable, plus the stuck-queue banner.
package invocations

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

// perGroup bounds how many invocations a group keeps.
const perGroup = 100

// activityWindow coarsens the activity sort so rows do not reshuffle on
// every invocation.
const activityWindow = 30 * time.Second

type Status int

const (
	Running Status = iota
	Succeeded
	Failed
)

type Invocation struct {
	RequestID  string
	FunctionID string
	WorkerID   string
	GroupKey   string
	Label      string
	Input      []byte
	Output     []byte
	Logs       []string
	ErrorType  string
	ErrorMsg   string
	Trace      []string
	Start      time.Time
	End        time.Time
	Cold       bool
	Status     Status
}

// Duration is the run time so far, or the final one.
func (i *Invocation) Duration(now time.Time) time.Duration {
	if i.End.IsZero() {
		return now.Sub(i.Start)
	}
	return i.End.Sub(i.Start)
}

type Group struct {
	Key         string
	Title       string
	Subtitle    string
	Invocations []*Invocation
	Total       int
	Errors      int
	Running     int
	Last        time.Time
}

// Match is one log line that matched a search.
type Match struct {
	Group      *Group
	Invocation *Invocation
	Line       int
	Text       string
}

type Store struct {
	now       func() time.Time
	groups    map[string]*Group
	requests  map[string]*Invocation
	workers   map[string]bool
	handlers  map[string]string
	Stalls    []aws.QueueStall
	LastPurge *aws.QueuePurgedEvent
	Total     int
	Errors    int
}

func NewStore(now func() time.Time) *Store {
	return &Store{
		now:      now,
		groups:   map[string]*Group{},
		requests: map[string]*Invocation{},
		workers:  map[string]bool{},
		handlers: map[string]string{},
	}
}

// Apply folds one dev event into the store and reports whether it changed.
func (s *Store) Apply(evt interface{}) bool {
	switch evt := evt.(type) {
	case *project.CompleteEvent:
		for _, resource := range evt.Resources {
			if resource.Type != "sst:aws:Function" {
				continue
			}
			if meta, ok := resource.Outputs["_metadata"].(map[string]interface{}); ok {
				if handler, ok := meta["handler"].(string); ok {
					s.handlers[resource.URN.Name()] = strings.TrimPrefix(handler, "./")
				}
			}
		}
		return false
	case *aws.FunctionInvokedEvent:
		s.invoked(evt)
	case *aws.FunctionLogEvent:
		inv, ok := s.requests[evt.RequestID]
		if !ok {
			return false
		}
		inv.Logs = append(inv.Logs, evt.Line)
	case *aws.FunctionResponseEvent:
		inv, ok := s.requests[evt.RequestID]
		if !ok {
			return false
		}
		inv.Output = evt.Output
		s.finish(inv, Succeeded)
	case *aws.FunctionErrorEvent:
		inv, ok := s.requests[evt.RequestID]
		if !ok {
			return false
		}
		inv.ErrorType, inv.ErrorMsg, inv.Trace = evt.ErrorType, evt.ErrorMessage, evt.Trace
		s.finish(inv, Failed)
	case *aws.QueueStallsEvent:
		s.Stalls = evt.Stalls
	case *aws.QueuePurgedEvent:
		s.LastPurge = evt
	default:
		return false
	}
	return true
}

func (s *Store) invoked(evt *aws.FunctionInvokedEvent) {
	key, title, subtitle := evt.FunctionID, s.functionTitle(evt.FunctionID), evt.FunctionID
	if evt.Handler != "" {
		key, title = "handler:"+evt.Handler, evt.Handler
		subtitle = evt.Label
		if subtitle == evt.Handler {
			subtitle = ""
		}
	}
	group, ok := s.groups[key]
	if !ok {
		group = &Group{Key: key, Title: title, Subtitle: subtitle}
		s.groups[key] = group
	}
	inv := &Invocation{
		RequestID:  evt.RequestID,
		FunctionID: evt.FunctionID,
		WorkerID:   evt.WorkerID,
		GroupKey:   key,
		Label:      evt.Label,
		Input:      evt.Input,
		Start:      s.now(),
		Cold:       !s.workers[evt.WorkerID],
		Status:     Running,
	}
	s.workers[evt.WorkerID] = true
	s.requests[evt.RequestID] = inv
	group.Invocations = append(group.Invocations, inv)
	if len(group.Invocations) > perGroup {
		dropped := group.Invocations[0]
		group.Invocations = group.Invocations[1:]
		if dropped.Status == Running {
			delete(s.requests, dropped.RequestID)
			group.Running--
		}
	}
	group.Total++
	group.Running++
	group.Last = inv.Start
	s.Total++
}

func (s *Store) finish(inv *Invocation, status Status) {
	delete(s.requests, inv.RequestID)
	inv.Status = status
	inv.End = s.now()
	group := s.groups[inv.GroupKey]
	group.Running--
	group.Last = inv.End
	if status == Failed {
		group.Errors++
		s.Errors++
	}
}

func (s *Store) functionTitle(functionID string) string {
	if handler, ok := s.handlers[functionID]; ok {
		return handler
	}
	return functionID
}

// Groups lists the groups whose title or subtitle fuzzily matches filter,
// most recently active first in coarse windows, then by title.
func (s *Store) Groups(filter string) []*Group {
	result := []*Group{}
	for _, group := range s.groups {
		if filter == "" || fuzzy(filter, group.Title) || fuzzy(filter, group.Subtitle) {
			result = append(result, group)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].Last.Truncate(activityWindow), result[j].Last.Truncate(activityWindow)
		if !a.Equal(b) {
			return a.After(b)
		}
		return result[i].Title < result[j].Title
	})
	return result
}

// Group returns a group by key.
func (s *Store) Group(key string) *Group {
	return s.groups[key]
}

// List lists a group's invocations newest first, narrowed by filter
// against their preview.
func (g *Group) List(filter string) []*Invocation {
	result := []*Invocation{}
	for i := len(g.Invocations) - 1; i >= 0; i-- {
		inv := g.Invocations[i]
		if filter == "" || fuzzy(filter, Preview(inv)) || fuzzy(filter, inv.RequestID) {
			result = append(result, inv)
		}
	}
	return result
}

// Search finds log, error and input text containing query (case
// insensitive), newest invocation first.
func (s *Store) Search(query string) []Match {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	matches := []Match{}
	for _, group := range s.Groups("") {
		for i := len(group.Invocations) - 1; i >= 0; i-- {
			inv := group.Invocations[i]
			for line, text := range inv.Logs {
				if strings.Contains(strings.ToLower(text), query) {
					matches = append(matches, Match{Group: group, Invocation: inv, Line: line, Text: text})
				}
			}
			if inv.ErrorMsg != "" && strings.Contains(strings.ToLower(inv.ErrorMsg), query) {
				matches = append(matches, Match{Group: group, Invocation: inv, Line: -1, Text: inv.ErrorType + ": " + inv.ErrorMsg})
			}
		}
	}
	return matches
}

// fuzzy reports whether every rune of query appears in text in order,
// ignoring case.
func fuzzy(query string, text string) bool {
	query, text = strings.ToLower(query), strings.ToLower(text)
	for _, r := range query {
		index := strings.IndexRune(text, r)
		if index < 0 {
			return false
		}
		text = text[index+len(string(r)):]
	}
	return true
}

// Preview is a one-line summary of an invocation's input: the method and
// path of an HTTP event, the queue and size of an SQS batch, the source of a
// scheduled event, or the start of the raw input.
func Preview(inv *Invocation) string {
	var event struct {
		RawPath        string `json:"rawPath"`
		Path           string `json:"path"`
		HTTPMethod     string `json:"httpMethod"`
		RouteKey       string `json:"routeKey"`
		RequestContext struct {
			HTTP struct {
				Method string `json:"method"`
			} `json:"http"`
		} `json:"requestContext"`
		Records []struct {
			EventSource    string `json:"eventSource"`
			EventSourceARN string `json:"eventSourceARN"`
		} `json:"Records"`
		Source     string `json:"source"`
		DetailType string `json:"detail-type"`
	}
	if json.Unmarshal(inv.Input, &event) == nil {
		switch {
		case event.RawPath != "":
			method := event.RequestContext.HTTP.Method
			return strings.TrimSpace(method + " " + event.RawPath)
		case event.Path != "":
			return strings.TrimSpace(event.HTTPMethod + " " + event.Path)
		case len(event.Records) > 0 && event.Records[0].EventSource == "aws:sqs":
			arn := strings.Split(event.Records[0].EventSourceARN, ":")
			return fmt.Sprintf("sqs %s ×%d", arn[len(arn)-1], len(event.Records))
		case event.DetailType != "":
			return event.Source + " " + event.DetailType
		}
	}
	text := strings.Join(strings.Fields(string(inv.Input)), " ")
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}

// PrettyJSON indents JSON, or returns the input as is.
func PrettyJSON(data []byte) string {
	var value interface{}
	if len(data) == 0 {
		return ""
	}
	if json.Unmarshal(data, &value) != nil {
		return string(data)
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(pretty)
}
