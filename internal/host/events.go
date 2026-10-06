package host

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// Event types of the instance event stream (op "events").
const (
	EventState        = "state"
	EventPorts        = "ports"
	EventLog          = "log"
	EventExecStarted  = "exec_started"
	EventExecExited   = "exec_exited"
	EventClient       = "client"
	EventDropped      = "dropped"
	DefaultEventQueue = 256 // per-subscriber queue before events are dropped
	EventHistorySize  = 500 // log events kept for new subscribers
)

// Instance states.
const (
	StateStarting     = "starting"
	StateConnecting   = "connecting"
	StateReady        = "ready"
	StateReconnecting = "reconnecting"
	StateStopping     = "stopping"
	StateStopped      = "stopped"
	StateFailed       = "failed"
)

// Log event sources and levels.
const (
	SourceHost         = "host"
	SourceEndpoint     = "endpoint"
	SourceDevcontainer = "devcontainer"
	LevelInfo          = "info"
	LevelWarn          = "warn"
	LevelError         = "error"
)

// Event is one entry of the instance event stream. Which fields are set
// depends on Type; MarshalJSON writes exactly the fields of that type.
type Event struct {
	T        time.Time   `json:"t"`
	Type     string      `json:"type"`
	State    string      `json:"state,omitempty"`
	Reason   string      `json:"reason,omitempty"`
	Ports    []StatePort `json:"ports,omitempty"`
	Source   string      `json:"source,omitempty"`
	Level    string      `json:"level,omitempty"`
	Message  string      `json:"message,omitempty"`
	ID       string      `json:"id,omitempty"`
	Argv     []string    `json:"argv,omitempty"`
	TTY      bool        `json:"tty,omitempty"`
	Code     int         `json:"code,omitempty"`
	Signal   string      `json:"signal,omitempty"`
	Attached int         `json:"attached,omitempty"`
	Count    int         `json:"count,omitempty"`
}

type eventHead struct {
	T    time.Time `json:"t"`
	Type string    `json:"type"`
}

// MarshalJSON writes the fields of the event's type, including zero values
// (e.g. "ports":[] or "code":0).
func (e Event) MarshalJSON() ([]byte, error) {
	head := eventHead{T: e.T, Type: e.Type}
	switch e.Type {
	case EventState:
		return json.Marshal(struct {
			eventHead
			State  string `json:"state"`
			Reason string `json:"reason,omitempty"`
		}{head, e.State, e.Reason})
	case EventPorts:
		ports := e.Ports
		if ports == nil {
			ports = []StatePort{}
		}
		return json.Marshal(struct {
			eventHead
			Ports []StatePort `json:"ports"`
		}{head, ports})
	case EventLog:
		return json.Marshal(struct {
			eventHead
			Source  string `json:"source"`
			Level   string `json:"level"`
			Message string `json:"message"`
		}{head, e.Source, e.Level, e.Message})
	case EventExecStarted:
		return json.Marshal(struct {
			eventHead
			ID   string   `json:"id"`
			Argv []string `json:"argv"`
			TTY  bool     `json:"tty"`
		}{head, e.ID, e.Argv, e.TTY})
	case EventExecExited:
		return json.Marshal(struct {
			eventHead
			ID     string `json:"id"`
			Code   int    `json:"code"`
			Signal string `json:"signal"`
		}{head, e.ID, e.Code, e.Signal})
	case EventClient:
		return json.Marshal(struct {
			eventHead
			Attached int `json:"attached"`
		}{head, e.Attached})
	case EventDropped:
		return json.Marshal(struct {
			eventHead
			Count int `json:"count"`
		}{head, e.Count})
	}
	type plain Event
	return json.Marshal(plain(e))
}

// EventBus fans instance events out to subscribers. Publishing never blocks:
// each subscriber has a bounded queue, and events that do not fit are counted
// and reported as one "dropped" event once the queue has room again.
type EventBus struct {
	now   func() time.Time
	queue int

	mu      sync.Mutex
	closed  bool
	state   *Event
	ports   *Event
	history []Event // ring buffer of log events
	next    int
	full    bool
	subs    map[*Subscription]struct{}
}

// Subscription receives events in order until it is closed.
type Subscription struct {
	// Snapshot is the number of snapshot events at the start of Events.
	Snapshot int

	bus     *EventBus
	ch      chan Event
	dropped int // guarded by bus.mu
	closed  bool
}

// NewEventBus returns a bus keeping EventHistorySize log events.
func NewEventBus() *EventBus {
	return &EventBus{
		now:     time.Now,
		queue:   DefaultEventQueue,
		history: make([]Event, EventHistorySize),
		subs:    make(map[*Subscription]struct{}),
	}
}

// Publish stamps and distributes an event.
func (b *EventBus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.T.IsZero() {
		e.T = b.now().UTC()
	}
	switch e.Type {
	case EventState:
		b.state = &e
	case EventPorts:
		b.ports = &e
	case EventLog:
		b.history[b.next] = e
		b.next = (b.next + 1) % len(b.history)
		if b.next == 0 {
			b.full = true
		}
	}
	for sub := range b.subs {
		sub.offer(e)
	}
}

func (s *Subscription) offer(e Event) {
	if s.dropped > 0 {
		select {
		case s.ch <- Event{T: e.T, Type: EventDropped, Count: s.dropped}:
			s.dropped = 0
		default:
			s.dropped++
			return
		}
	}
	select {
	case s.ch <- e:
	default:
		s.dropped++
	}
}

// Subscribe returns a subscription that starts with a snapshot: the current
// state, the current ports, and up to history recent log events. After Close,
// the subscription holds only the snapshot.
func (b *EventBus) Subscribe(history int) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	var snapshot []Event
	if b.state != nil {
		snapshot = append(snapshot, *b.state)
	}
	if b.ports != nil {
		snapshot = append(snapshot, *b.ports)
	}
	snapshot = append(snapshot, b.recentLocked(history)...)
	sub := &Subscription{bus: b, ch: make(chan Event, b.queue+len(snapshot))}
	for _, e := range snapshot {
		sub.ch <- e
	}
	sub.Snapshot = len(sub.ch)
	if b.closed {
		sub.closed = true
		close(sub.ch)
		return sub
	}
	b.subs[sub] = struct{}{}
	return sub
}

func (b *EventBus) recentLocked(n int) []Event {
	count := b.next
	if b.full {
		count = len(b.history)
	}
	if n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}
	result := make([]Event, 0, n)
	start := (b.next - n + len(b.history)) % len(b.history)
	for i := 0; i < n; i++ {
		result = append(result, b.history[(start+i)%len(b.history)])
	}
	return result
}

// Events returns the subscription's channel; it is closed by Close or when
// the bus closes.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close ends the subscription.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(s.bus.subs, s)
	close(s.ch)
}

// Close ends all subscriptions after their queued events. Later publishes
// only update the snapshot.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for sub := range b.subs {
		sub.closed = true
		close(sub.ch)
	}
	b.subs = map[*Subscription]struct{}{}
}

// Subscribers returns the number of open subscriptions.
func (b *EventBus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// eventLogWriter turns written log lines into log events of one source and
// writes them, timestamped and prefixed, to out.
type eventLogWriter struct {
	bus    *EventBus
	out    io.Writer
	source string
	prefix string // added to lines written to out
	now    func() time.Time

	mu      sync.Mutex
	partial []byte
}

func newEventLogWriter(bus *EventBus, out io.Writer, source, prefix string) *eventLogWriter {
	return &eventLogWriter{bus: bus, out: out, source: source, prefix: prefix, now: time.Now}
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.emitLocked(string(bytes.TrimRight(w.partial[:i], "\r")), "")
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > execLogMaxLine {
		w.emitLocked(string(w.partial), "")
		w.partial = nil
	}
	return len(p), nil
}

// Log emits one message with an explicit level ("" infers it).
func (w *eventLogWriter) Log(level, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		w.emitLocked(strings.TrimRight(line, "\r"), level)
	}
}

func (w *eventLogWriter) emitLocked(line, level string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if level == "" {
		level = inferLogLevel(line)
	}
	now := w.now()
	if w.bus != nil {
		w.bus.Publish(Event{T: now.UTC(), Type: EventLog, Source: w.source, Level: level, Message: line})
	}
	if w.out != nil {
		io.WriteString(w.out, now.Format("2006/01/02 15:04:05 ")+w.prefix+line+"\n")
	}
}

// inferLogLevel classifies a free-form log line.
func inferLogLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "warning"):
		return LevelWarn
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"), strings.Contains(lower, "panic"):
		return LevelError
	default:
		return LevelInfo
	}
}
