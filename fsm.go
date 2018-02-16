package marionette

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strconv"

	"github.com/redjack/marionette/fte"
	"github.com/redjack/marionette/mar"
	"go.uber.org/zap"
)

var (
	// ErrNoTransitions is returned from FSM.Next() when no transitions can be found.
	ErrNoTransitions = errors.New("no transitions available")

	// ErrRetryTransition is returned from FSM.Next() when a transition should be reattempted.
	ErrRetryTransition = errors.New("retry transition")

	ErrUUIDMismatch = errors.New("uuid mismatch")
)

// FSM represents an interface for the Marionette state machine.
type FSM interface {
	// Document & FSM identifiers.
	UUID() int
	SetInstanceID(int)
	InstanceID() int

	// Party & networking.
	Party() string
	Host() string
	Port() int

	// The current state in the FSM.
	State() string

	// Returns true if State() == 'dead'
	Dead() bool

	// Moves to the next available state.
	// Returns ErrNoTransition if there is no state to move to.
	Next(ctx context.Context) error

	// Moves through the entire state machine until it reaches 'dead' state.
	Execute(ctx context.Context) error

	// Restarts the FSM so it can be reused.
	Reset()

	// Returns an FTE cipher or DFA from the cache or creates a new one.
	Cipher(regex string) Cipher
	DFA(regex string, msgLen int) DFA

	// Returns the network connection attached to the FSM.
	Conn() *BufferedConn

	// Listen opens a new listener to accept data and drains into the buffer.
	Listen() (int, error)

	// Returns the stream set attached to the FSM.
	StreamSet() *StreamSet

	// Sets and retrieves key/values from the FSM.
	SetVar(key string, value interface{})
	Var(key string) interface{}

	// Returns a copy of the FSM with a different format.
	Clone(doc *mar.Document) FSM
}

// Ensure implementation implements interface.
var _ FSM = &fsm{}

// fsm is the default implementation of the FSM.
type fsm struct {
	doc      *mar.Document
	host     string
	party    string
	fteCache *fte.Cache

	conn       *BufferedConn
	streamSet  *StreamSet
	listeners  map[int]net.Listener
	closeFuncs []func() error

	state string
	stepN int
	rand  *rand.Rand

	// Lookup of transitions by src state.
	transitions map[string][]*mar.Transition

	vars map[string]interface{}

	// Set by the first sender and used to seed PRNG.
	instanceID int
}

// NewFSM returns a new FSM. If party is the first sender then the instance id is set.
func NewFSM(doc *mar.Document, host, party string, conn net.Conn, streamSet *StreamSet) FSM {
	fsm := &fsm{
		state:     "start",
		vars:      make(map[string]interface{}),
		doc:       doc,
		host:      host,
		party:     party,
		fteCache:  fte.NewCache(),
		conn:      NewBufferedConn(conn, MaxCellLength),
		streamSet: streamSet,
		listeners: make(map[int]net.Listener),
	}
	fsm.buildTransitions()
	fsm.initFirstSender()
	return fsm
}

func (fsm *fsm) buildTransitions() {
	fsm.transitions = make(map[string][]*mar.Transition)
	for _, t := range fsm.doc.Transitions {
		fsm.transitions[t.Source] = append(fsm.transitions[t.Source], t)
	}
}

func (fsm *fsm) initFirstSender() {
	if fsm.party != fsm.doc.FirstSender() {
		return
	}
	fsm.instanceID = int(rand.Int31())
	fsm.rand = rand.New(rand.NewSource(int64(fsm.instanceID)))
}

func (fsm *fsm) Reset() {
	fsm.state = "start"
	fsm.vars = make(map[string]interface{})

	for _, fn := range fsm.closeFuncs {
		if err := fn(); err != nil {
			fsm.logger().Error("close error", zap.Error(err))
		}
	}
	fsm.closeFuncs = nil
}

// UUID returns the computed MAR document UUID.
func (fsm *fsm) UUID() int { return fsm.doc.UUID }

// InstanceID returns the ID for this specific FSM.
func (fsm *fsm) InstanceID() int { return fsm.instanceID }

// SetInstanceID sets the ID for the FSM.
func (fsm *fsm) SetInstanceID(id int) { fsm.instanceID = id }

// State returns the current state of the FSM.
func (fsm *fsm) State() string { return fsm.state }

// Conn returns the connection the FSM was initialized with.
func (fsm *fsm) Conn() *BufferedConn { return fsm.conn }

// StreamSet returns the stream set the FSM was initialized with.
func (fsm *fsm) StreamSet() *StreamSet { return fsm.streamSet }

// Host returns the hostname the FSM was initialized with.
func (fsm *fsm) Host() string { return fsm.host }

// Party returns "client" or "server" depending on who is initializing the FSM.
func (fsm *fsm) Party() string { return fsm.party }

// Port returns the port from the underlying document.
// If port is a named port then it is looked up in the local variables.
func (fsm *fsm) Port() int {
	if port, err := strconv.Atoi(fsm.doc.Port); err == nil {
		return port
	}

	if v := fsm.Var(fsm.doc.Port); v != nil {
		port, _ := v.(int)
		return port
	}

	return 0
}

// Dead returns true when the FSM is complete.
func (fsm *fsm) Dead() bool { return fsm.state == "dead" }

// Execute runs the the FSM to completion.
func (fsm *fsm) Execute(ctx context.Context) error {
	// If no connection is passed in, create one.
	// This occurs when an FSM is spawned.
	if err := fsm.ensureConn(ctx); err != nil {
		return err
	}

	for !fsm.Dead() {
		if err := fsm.Next(ctx); err == ErrRetryTransition {
			fsm.logger().Debug("retry transition", zap.String("state", fsm.State()))
			continue
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (fsm *fsm) Next(ctx context.Context) (err error) {
	// Generate a new PRNG once we have an instance ID.
	if err := fsm.init(); err != nil {
		return err
	}

	// If we have a successful transition, update our state info.
	// Exit if no transitions were successful.
	nextState, err := fsm.next(true)
	if err != nil {
		return err
	}

	fsm.stepN += 1
	fsm.state = nextState

	return nil
}

func (fsm *fsm) next(eval bool) (nextState string, err error) {
	// Find all possible transitions from the current state.
	transitions := mar.FilterTransitionsBySource(fsm.doc.Transitions, fsm.state)
	errorTransitions := mar.FilterErrorTransitions(transitions)

	// Then filter by PRNG (if available) or return all (if unavailable).
	transitions = mar.FilterNonErrorTransitions(transitions)
	transitions = mar.ChooseTransitions(transitions, fsm.rand)
	assert(len(transitions) > 0)

	// Add error transitions back in after selection.
	transitions = append(transitions, errorTransitions...)

	// Attempt each possible transition.
	for _, transition := range transitions {
		// If there's no action block then move to the next state.
		if transition.ActionBlock == "NULL" {
			return transition.Destination, nil
		}

		// Find all actions for this destination and current party.
		blk := fsm.doc.ActionBlock(transition.ActionBlock)
		if blk == nil {
			return "", fmt.Errorf("fsm.Next(): action block not found: %q", transition.ActionBlock)
		}
		actions := mar.FilterActionsByParty(blk.Actions, fsm.party)

		// Attempt to execute each action.
		if eval {
			if err := fsm.evalActions(actions); err != nil {
				return "", err
			}
		}
		return transition.Destination, nil
	}
	return "", nil
}

// init initializes the PRNG if we now have a instance id.
func (fsm *fsm) init() (err error) {
	if fsm.rand != nil || fsm.instanceID == 0 {
		return nil
	}

	// Create new PRNG.
	fsm.rand = rand.New(rand.NewSource(int64(fsm.instanceID)))

	// Restart FSM from the beginning and iterate until the current step.
	fsm.state = "start"
	for i := 0; i < fsm.stepN; i++ {
		fsm.state, err = fsm.next(false)
		if err != nil {
			return err
		}
		assert(fsm.state != "")
	}
	return nil
}

func (fsm *fsm) evalActions(actions []*mar.Action) error {
	if len(actions) == 0 {
		return nil
	}

	for _, action := range actions {
		// If there is no matching regex then simply evaluate action.
		if action.Regex != "" {
			// Compile regex.
			re, err := regexp.Compile(action.Regex)
			if err != nil {
				return err
			}

			// Only evaluate action if buffer matches.
			buf, err := fsm.conn.Peek(-1)
			if err != nil {
				return err
			} else if !re.Match(buf) {
				continue
			}
		}

		fn := FindPlugin(action.Module, action.Method)
		if fn == nil {
			return fmt.Errorf("plugin not found: %s", action.Name())
		} else if err := fn(fsm, action.ArgValues()...); err != nil {
			return err
		}
		return nil
	}

	return ErrNoTransitions
}

func (fsm *fsm) Var(key string) interface{} {
	switch key {
	case "model_instance_id":
		return fsm.InstanceID
	case "model_uuid":
		return fsm.doc.UUID
	case "party":
		return fsm.party
	default:
		return fsm.vars[key]
	}
}

func (fsm *fsm) SetVar(key string, value interface{}) {
	fsm.vars[key] = value
}

// Cipher returns a cipher with the given settings.
// If no cipher exists then a new one is created and returned.
func (fsm *fsm) Cipher(regex string) Cipher {
	return fsm.fteCache.Cipher(regex)
}

// DFA returns a DFA with the given settings.
// If no DFA exists then a new one is created and returned.
func (fsm *fsm) DFA(regex string, n int) DFA {
	return fsm.fteCache.DFA(regex, n)
}

func (fsm *fsm) Listen() (port int, err error) {
	addr := fsm.host
	if s := os.Getenv("MARIONETTE_CHANNEL_BIND_PORT"); s != "" {
		addr = net.JoinHostPort(addr, s)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	port = ln.Addr().(*net.TCPAddr).Port
	fsm.listeners[port] = ln
	fsm.closeFuncs = append(fsm.closeFuncs, ln.Close)

	return port, nil
}

func (fsm *fsm) ensureConn(ctx context.Context) error {
	if fsm.conn != nil {
		return nil
	}
	if fsm.party == PartyClient {
		return fsm.ensureClientConn(ctx)
	}
	return fsm.ensureServerConn(ctx)
}

func (fsm *fsm) ensureClientConn(ctx context.Context) error {
	conn, err := net.Dial(fsm.doc.Transport, net.JoinHostPort(fsm.host, strconv.Itoa(fsm.Port())))
	if err != nil {
		return err
	}

	fsm.conn = NewBufferedConn(conn, MaxCellLength)
	fsm.closeFuncs = append(fsm.closeFuncs, conn.Close)

	return nil
}

func (fsm *fsm) ensureServerConn(ctx context.Context) error {
	ln := fsm.listeners[fsm.Port()]
	if ln == nil {
		return fmt.Errorf("marionette.FSM: no listeners on port %d", fsm.Port())
	}

	conn, err := ln.Accept()
	if err != nil {
		return err
	}

	fsm.conn = NewBufferedConn(conn, MaxCellLength)
	fsm.closeFuncs = append(fsm.closeFuncs, conn.Close)

	return nil
}

func (f *fsm) Clone(doc *mar.Document) FSM {
	other := &fsm{
		state:     "start",
		vars:      make(map[string]interface{}),
		doc:       doc,
		host:      f.host,
		party:     f.party,
		fteCache:  f.fteCache,
		streamSet: f.streamSet,
		listeners: f.listeners,
	}

	other.buildTransitions()
	other.initFirstSender()

	other.vars = make(map[string]interface{})
	for k, v := range f.vars {
		other.vars[k] = v
	}

	return other
}

func (fsm *fsm) logger() *zap.Logger {
	return Logger.With(zap.String("party", fsm.party))
}
