package host

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestEventBusSnapshotAndLive(t *testing.T) {
	bus := NewEventBus()
	bus.Publish(Event{Type: EventState, State: StateStarting})
	bus.Publish(Event{Type: EventState, State: StateReady})
	bus.Publish(Event{Type: EventPorts, Ports: []StatePort{{Port: 3000, Address: "127.0.0.1", LocalPort: 3000}}})
	for i := range 5 {
		bus.Publish(Event{Type: EventLog, Source: SourceHost, Level: LevelInfo, Message: fmt.Sprintf("line %d", i)})
	}

	sub := bus.Subscribe(2)
	defer sub.Close()
	if sub.Snapshot != 4 {
		t.Fatalf("snapshot size = %d, want state + ports + 2 logs", sub.Snapshot)
	}
	want := []string{"state:ready", "ports:", "log:line 3", "log:line 4"}
	for _, w := range want {
		e := <-sub.Events()
		if got := e.Type + ":" + e.State + e.Message; got != w {
			t.Fatalf("snapshot event = %q, want %q", got, w)
		}
	}
	bus.Publish(Event{Type: EventLog, Source: SourceHost, Level: LevelInfo, Message: "live"})
	if e := <-sub.Events(); e.Message != "live" || e.T.IsZero() {
		t.Fatalf("live event = %+v", e)
	}

	bus.Close()
	if _, open := <-sub.Events(); open {
		t.Fatal("subscription open after bus close")
	}
	late := bus.Subscribe(0)
	if e := <-late.Events(); e.State != StateReady {
		t.Fatalf("snapshot after close = %+v", e)
	}
}

func TestEventBusHistoryIsBounded(t *testing.T) {
	bus := NewEventBus()
	for i := range EventHistorySize + 10 {
		bus.Publish(Event{Type: EventLog, Message: fmt.Sprintf("%d", i)})
	}
	sub := bus.Subscribe(EventHistorySize + 100)
	if sub.Snapshot != EventHistorySize {
		t.Fatalf("snapshot = %d, want %d", sub.Snapshot, EventHistorySize)
	}
	if e := <-sub.Events(); e.Message != "10" {
		t.Fatalf("oldest kept event = %q, want 10", e.Message)
	}
}

func TestEventBusSlowSubscriberGetsDropMarker(t *testing.T) {
	bus := NewEventBus()
	bus.queue = 3
	sub := bus.Subscribe(0)
	fast := bus.Subscribe(0)
	go func() {
		for range fast.Events() {
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := range 10 {
			bus.Publish(Event{Type: EventLog, Message: fmt.Sprintf("%d", i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}

	var got []string
	for range 3 {
		e := <-sub.Events()
		got = append(got, e.Message)
	}
	bus.Publish(Event{Type: EventLog, Message: "after"})
	dropped := <-sub.Events()
	after := <-sub.Events()
	if fmt.Sprint(got) != "[0 1 2]" || dropped.Type != EventDropped || dropped.Count != 7 || after.Message != "after" {
		t.Fatalf("got %v, then %+v, %+v", got, dropped, after)
	}
}

func TestEventJSONHasTypeFields(t *testing.T) {
	ts := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		event Event
		want  string
	}{
		{Event{T: ts, Type: EventState, State: StateReady}, `{"t":"2026-10-07T10:00:00Z","type":"state","state":"ready"}`},
		{Event{T: ts, Type: EventPorts}, `{"t":"2026-10-07T10:00:00Z","type":"ports","ports":[]}`},
		{Event{T: ts, Type: EventLog, Source: SourceEndpoint, Level: LevelWarn, Message: "m"}, `{"t":"2026-10-07T10:00:00Z","type":"log","source":"endpoint","level":"warn","message":"m"}`},
		{Event{T: ts, Type: EventExecStarted, ID: "a", Argv: []string{"opencode", "acp"}}, `{"t":"2026-10-07T10:00:00Z","type":"exec_started","id":"a","argv":["opencode","acp"],"tty":false}`},
		{Event{T: ts, Type: EventExecExited, ID: "a"}, `{"t":"2026-10-07T10:00:00Z","type":"exec_exited","id":"a","code":0,"signal":""}`},
		{Event{T: ts, Type: EventClient}, `{"t":"2026-10-07T10:00:00Z","type":"client","attached":0}`},
		{Event{T: ts, Type: EventDropped, Count: 4}, `{"t":"2026-10-07T10:00:00Z","type":"dropped","count":4}`},
	} {
		data, err := json.Marshal(tt.event)
		if err != nil || string(data) != tt.want {
			t.Errorf("json = %s, %v\nwant   %s", data, err, tt.want)
		}
		var back Event
		if err := json.Unmarshal(data, &back); err != nil || back.Type != tt.event.Type {
			t.Errorf("round trip of %s = %+v, %v", data, back, err)
		}
	}
}

func TestEventLogWriterInfersLevels(t *testing.T) {
	bus := NewEventBus()
	sub := bus.Subscribe(0)
	w := newEventLogWriter(bus, nil, SourceHost, "")
	fmt.Fprintf(w, "Container started\nWarning: SSH agent forwarding disabled\nFailed to forward port 3000\npartial")
	w.Write([]byte(" line\n"))
	for _, want := range []string{"info:Container started", "warn:Warning: SSH agent forwarding disabled", "error:Failed to forward port 3000", "info:partial line"} {
		e := <-sub.Events()
		if got := e.Level + ":" + e.Message; got != want || e.Source != SourceHost {
			t.Fatalf("event = %+v, want %q", e, want)
		}
	}
}
