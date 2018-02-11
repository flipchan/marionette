package tg

import (
	"math/big"

	"github.com/redjack/marionette"
	"github.com/redjack/marionette/fte"
)

type RankerCipher struct {
	key   string
	regex string
	dfa   *fte.DFA
}

func NewRankerCipher(key, regex string, msgLen int) *RankerCipher {
	return &RankerCipher{
		key:   key,
		regex: regex,
		dfa:   fte.NewDFA(regex, msgLen),
	}
}

func (c *RankerCipher) Key() string {
	return c.key
}

func (c *RankerCipher) Capacity() int {
	return c.dfa.Capacity()
}

func (c *RankerCipher) Encrypt(fsm marionette.FSM, template string, data []byte) (ciphertext []byte, err error) {
	rank := &big.Int{}
	rank.SetBytes(data)

	ret, err := c.dfa.Unrank(rank)
	if err != nil {
		return nil, err
	}
	return []byte(ret), nil
}

func (c *RankerCipher) Decrypt(fsm marionette.FSM, ciphertext []byte) (plaintext []byte, err error) {
	rank, err := c.dfa.Rank(string(ciphertext))
	if err != nil {
		return nil, err
	}
	return rank.Bytes(), nil
}
