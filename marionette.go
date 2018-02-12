package marionette

import (
	"math/big"
	"math/rand"
	"time"

	"go.uber.org/zap"
)

const (
	PartyClient = "client"
	PartyServer = "server"
)

func init() {
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.TimeKey = ""
	config.EncoderConfig.CallerKey = ""
	Logger, _ = config.Build()
}

// Logger is the global marionette logger.
var Logger = zap.NewNop()

// Rand returns a new PRNG seeded from the current time.
// This function can be overridden by the tests to provide a repeatable PRNG.
var Rand = func() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }

// PluginFunc represents a plugin in the MAR language.
type PluginFunc func(fsm FSM, args ...interface{}) (success bool, err error)

// FindPlugin returns a plugin function by module & name.
func FindPlugin(module, method string) PluginFunc {
	return plugins[pluginKey{module, method}]
}

// RegisterPlugin adds a plugin to the plugin registry.
// Panic on duplicate registration.
func RegisterPlugin(module, method string, fn PluginFunc) {
	if v := FindPlugin(module, method); v != nil {
		panic("plugin already registered")
	}
	plugins[pluginKey{module, method}] = fn
}

type pluginKey struct {
	module string
	method string
}

var plugins = make(map[pluginKey]PluginFunc)

// Cipher represents the interface to the FTE Cipher.
type Cipher interface {
	Capacity() (int, error)
	Encrypt(plaintext []byte) (ciphertext []byte, err error)
	Decrypt(ciphertext []byte) (plaintext, remainder []byte, err error)
}

// Ranker represents the interface to the DFA Ranker.
type Ranker interface {
	Capacity() int
	Rank(s string) (rank *big.Int, err error)
	Unrank(rank *big.Int) (ret string, err error)
	NumWordsInSlice(n int) (numWords int, err error)
}

func assert(condition bool) {
	if !condition {
		panic("assertion failed")
	}
}
