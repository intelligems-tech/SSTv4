package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Machine is a parsed Amazon States Language definition, or one branch of
// a Parallel or the item processor of a Map.
type Machine struct {
	StartAt string
	// Order lists the state names as the definition declares them.
	Order  []string
	States map[string]*State
}

// State is one state of a definition. Raw is the state's own JSON, which
// TestState runs on its own.
type State struct {
	Name     string
	Type     string
	Comment  string
	Resource string
	// Function is the Lambda a Task invokes, when it names one.
	Function string
	Next     string
	End      bool
	Default  string
	Choices  []string
	Catches  []string
	Branches []*Machine
	// Processor is a Map's Iterator or ItemProcessor.
	Processor *Machine
	Raw       json.RawMessage
}

// ParseDefinition parses an ASL definition.
func ParseDefinition(definition string) (*Machine, error) {
	return parseMachine([]byte(definition))
}

func parseMachine(data []byte) (*Machine, error) {
	var raw struct {
		StartAt string          `json:"StartAt"`
		States  json.RawMessage `json:"States"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	names, values, err := orderedObject(raw.States)
	if err != nil {
		return nil, fmt.Errorf("States: %w", err)
	}
	m := &Machine{StartAt: raw.StartAt, Order: names, States: map[string]*State{}}
	for i, name := range names {
		state, err := parseState(name, values[i])
		if err != nil {
			return nil, fmt.Errorf("state %q: %w", name, err)
		}
		m.States[name] = state
	}
	return m, nil
}

func parseState(name string, data json.RawMessage) (*State, error) {
	var raw struct {
		Type       string `json:"Type"`
		Comment    string `json:"Comment"`
		Resource   string `json:"Resource"`
		Next       string `json:"Next"`
		End        bool   `json:"End"`
		Default    string `json:"Default"`
		Parameters struct {
			FunctionName string `json:"FunctionName"`
		} `json:"Parameters"`
		Arguments struct {
			FunctionName string `json:"FunctionName"`
		} `json:"Arguments"`
		Choices []struct {
			Next string `json:"Next"`
		} `json:"Choices"`
		Catch []struct {
			Next string `json:"Next"`
		} `json:"Catch"`
		Branches      []json.RawMessage `json:"Branches"`
		Iterator      json.RawMessage   `json:"Iterator"`
		ItemProcessor json.RawMessage   `json:"ItemProcessor"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	s := &State{
		Name:     name,
		Type:     raw.Type,
		Comment:  raw.Comment,
		Resource: raw.Resource,
		Next:     raw.Next,
		End:      raw.End,
		Default:  raw.Default,
		Raw:      data,
	}
	s.Function = raw.Parameters.FunctionName
	if s.Function == "" {
		s.Function = raw.Arguments.FunctionName
	}
	if s.Function == "" && isLambdaArn(raw.Resource) {
		s.Function = raw.Resource
	}
	for _, choice := range raw.Choices {
		s.Choices = append(s.Choices, choice.Next)
	}
	for _, catch := range raw.Catch {
		s.Catches = append(s.Catches, catch.Next)
	}
	for i, branch := range raw.Branches {
		m, err := parseMachine(branch)
		if err != nil {
			return nil, fmt.Errorf("branch %d: %w", i, err)
		}
		s.Branches = append(s.Branches, m)
	}
	processor := raw.ItemProcessor
	if len(processor) == 0 {
		processor = raw.Iterator
	}
	if len(processor) > 0 {
		m, err := parseMachine(processor)
		if err != nil {
			return nil, fmt.Errorf("item processor: %w", err)
		}
		s.Processor = m
	}
	return s, nil
}

// Terminal reports whether the state ends its machine or branch.
func (s *State) Terminal() bool {
	return s.End || s.Type == "Succeed" || s.Type == "Fail"
}

// Children are the machines nested in a Parallel or Map.
func (s *State) Children() []*Machine {
	if s.Processor != nil {
		return []*Machine{s.Processor}
	}
	return s.Branches
}

// Testable reports whether TestState can run the state on its own; it
// cannot run a Parallel or Map, or a Task that waits on a job or a token.
func (s *State) Testable() bool {
	switch s.Type {
	case "Parallel", "Map":
		return false
	case "Task":
		return !strings.HasSuffix(s.Resource, ".sync") && !strings.Contains(s.Resource, ".sync:") && !strings.HasSuffix(s.Resource, ".waitForTaskToken")
	}
	return true
}

// Find looks a state up by name in the machine and every nested one; ASL
// names are unique across a whole definition.
func (m *Machine) Find(name string) *State {
	if s, ok := m.States[name]; ok {
		return s
	}
	for _, parent := range m.Order {
		for _, child := range m.States[parent].Children() {
			if s := child.Find(name); s != nil {
				return s
			}
		}
	}
	return nil
}

// Count is the number of states, nested ones included.
func (m *Machine) Count() int {
	count := len(m.Order)
	for _, name := range m.Order {
		for _, child := range m.States[name].Children() {
			count += child.Count()
		}
	}
	return count
}

// orderedObject splits a JSON object into its keys, in order, and values.
func orderedObject(data json.RawMessage) ([]string, []json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected an object")
	}
	names := []string{}
	values := []json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		names = append(names, name)
		values = append(values, value)
	}
	return names, values, nil
}
