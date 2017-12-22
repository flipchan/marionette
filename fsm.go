package marionette

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strconv"

	"github.com/redjack/marionette/mar"
)

var ErrNoTransition = errors.New("no matching transition")

type FSM struct {
	doc       *mar.Document
	party     string
	bufferSet *StreamBufferSet
	dec       *CellDecoder

	state string
	stepN int
	rand  *rand.Rand
	conn  *bufConn // channel

	// Lookup of transitions by src state.
	transitions map[string][]*mar.Transition

	vars    map[string]interface{}
	ciphers map[cipherKey]Cipher

	// Set by the first sender and used to seed PRNG.
	InstanceID int

	// Network dialer. Defaults to net.Dialer.
	Dialer Dialer

	// Factory functions to create new stateful ciphers.
	NewCipherFunc NewCipherFunc
}

// NewFSM returns a new FSM. If party is the first sender then the instance id is set.
func NewFSM(doc *mar.Document, party string, bufferSet *StreamBufferSet, dec *CellDecoder) *FSM {
	fsm := &FSM{
		doc:         doc,
		party:       party,
		state:       "start",
		bufferSet:   bufferSet,
		dec:         dec,
		vars:        make(map[string]interface{}),
		transitions: make(map[string][]*mar.Transition),
		ciphers:     make(map[cipherKey]Cipher),

		Dialer:        &net.Dialer{},
		NewCipherFunc: NewFTECipher,
	}

	// Build transition map.
	for _, t := range doc.Transitions {
		fsm.transitions[t.Source] = append(fsm.transitions[t.Source], t)
	}

	// The initial sender generates the instance ID.
	if party == doc.FirstSender() {
		fsm.InstanceID = int(rand.Int31())
		fsm.rand = rand.New(rand.NewSource(int64(fsm.InstanceID)))
	}

	return fsm
}

func (fsm *FSM) UUID() int {
	return fsm.doc.UUID
}

// State returns the current state of the FSM.
func (fsm *FSM) State() string {
	return fsm.state
}

// Port returns the port from the underlying document.
// If port is a named port then it is looked up in the local variables.
func (fsm *FSM) Port() int {
	if port, err := strconv.Atoi(fsm.doc.Port); err == nil {
		return port
	}

	// port, _ := fsm.locals[fsm.doc.Port].(int)
	// return port
	panic("TODO")
}

// SetConn sets the connection on the FSM.
func (fsm *FSM) SetConn(conn net.Conn) {
	fsm.conn = newBufConn(conn)
}

// Dead returns true when the FSM is complete.
func (fsm *FSM) Dead() bool { return fsm.state == "dead" }

func (fsm *FSM) Next(ctx context.Context) (err error) {
	// Create a new connection from the client if none is available.
	if fsm.party == PartyClient && fsm.conn == nil {
		const serverIP = "127.0.0.1" // TODO: Pass in.
		conn, err := fsm.Dialer.DialContext(ctx, fsm.doc.Transport, net.JoinHostPort(serverIP, fsm.doc.Port))
		if err != nil {
			return err
		}
		fsm.SetConn(conn)
	}

	// Exit if no connection available.
	if fsm.conn == nil {
		return errors.New("fsm.Next(): no connection available")
	}

	// Generate a new PRNG once we have an instance ID.
	if err := fsm.init(); err != nil {
		return err
	}

	// If we have a successful transition, update our state info.
	// Exit if no transitions were successful.
	if nextState, err := fsm.next(); err != nil {
		return err
	} else if nextState == "" {
		return ErrNoTransition
	} else {
		fsm.stepN += 1
		fsm.state = nextState
	}

	return nil
}

func (fsm *FSM) next() (nextState string, err error) {
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
		if matched, err := fsm.evalActions(actions); err != nil {
			return "", err
		} else if matched {
			return transition.Destination, nil
		}
	}
	return "", nil
}

// init initializes the PRNG if we now have a instance id.
func (fsm *FSM) init() (err error) {
	if fsm.rand != nil || fsm.InstanceID == 0 {
		return nil
	}

	// Create new PRNG.
	fsm.rand = rand.New(rand.NewSource(int64(fsm.InstanceID)))

	// Restart FSM from the beginning and iterate until the current step.
	fsm.state = "start"
	for i := 0; i < fsm.stepN; i++ {
		fsm.state, err = fsm.next()
		if err != nil {
			return err
		}
		assert(fsm.state != "")
	}
	return nil
}

func (fsm *FSM) next_transition(src_state, dst_state string) *mar.Transition {
	for _, transition := range fsm.transitions[src_state] {
		if transition.Destination == dst_state {
			return transition
		}
	}
	return nil
}

func (fsm *FSM) evalActions(actions []*mar.Action) (bool, error) {
	if len(actions) == 0 {
		return true, nil
	}

	for _, action := range actions {
		// If there is no matching regex then simply evaluate action.
		if action.Regex != "" {
			// Compile regex.
			// TODO(benbjohnson): Compile at parse time and store.
			re, err := regexp.Compile(action.Regex)
			if err != nil {
				return false, err
			}

			// Only evaluate action if buffer matches.
			incoming_buffer := fsm.conn.Peek()
			if !re.Match(incoming_buffer) {
				continue
			}
		}

		if success, err := fsm.evalAction(action); err != nil {
			return false, err
		} else if success {
			return true, nil
		}
		continue
	}

	return false, nil
}

func (fsm *FSM) evalAction(action *mar.Action) (bool, error) {
	fn := FindPlugin(action.Name, action.Method)
	if fn == nil {
		return false, fmt.Errorf("fsm.evalAction(): action not found: %s.%s", action.Name, action.Method)
	}
	return fn(fsm, action.ArgValues())
}

/*
func (fsm *FSM) do_precomputations() {
	for _, action := range fsm.actions_ {
		if action.module_ == "fte" && strings.HasPrefix(action.method_, "send") {
			fsm.get_fte_obj(action.Arg(0), action.Arg(1))
		}
	}
}


func transitionKeys(transitions map[string]PATransition) []string {
	a := make([]string, 0, len(transitions))
	for k := range transitions {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

func (fsm *FSM) isRunning() bool {
	return fsm.state != "dead"
}

func (fsm *FSM) add_state(name string) {
	if !stringSliceContains(fsm.states_.keys(), name) {
		fsm.states_[name] = PAState(name)
	}
}

func (fsm *FSM) set_multiplexer_outgoing(multiplexer *OutgoingBuffer) {
	fsm.global["multiplexer_outgoing"] = multiplexer
}

func (fsm *FSM) set_multiplexer_incoming(multiplexer *IncomingBuffer) {
	fsm.global["multiplexer_incoming"] = multiplexer
}

func (fsm *FSM) stop() {
	fsm.state = "dead"
}

func (fsm *FSM) set_port(port int) { // TODO: Maybe string?
	fsm.port_ = port
}

func (fsm *FSM) get_port() int {
	if fsm.port_ != 0 {
		return fsm.port_
	}
	return fsm.local[fsm.port_]
}

*/

func (fsm *FSM) Var(key string) interface{} {
	switch key {
	case "model_instance_id":
		return fsm.InstanceID
	case "model_uuid":
		return fsm.doc.UUID
	case "party":
		return fsm.party
	case "multiplexer_incoming":
		return fsm.dec
	case "multiplexer_outgoing":
		return fsm.bufferSet
	default:
		return fsm.vars[key]
	}
}

func (fsm *FSM) SetVar(key string, value interface{}) {
	fsm.vars[key] = value
}

// Cipher returns a cipher with the given settings.
// If no cipher exists then a new one is created and returned.
func (fsm *FSM) Cipher(regex string, msgLen int) (_ Cipher, err error) {
	key := cipherKey{regex, msgLen}
	cipher := fsm.ciphers[key]
	if cipher != nil {
		return cipher, nil
	}

	cipher, err = fsm.NewCipherFunc(regex, msgLen)
	if err != nil {
		return nil, err
	}
	fsm.ciphers[key] = cipher
	return cipher, nil
}

type cipherKey struct {
	regex  string
	msgLen int
}