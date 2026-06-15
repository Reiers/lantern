package evm

import (
	"errors"

	"github.com/holiman/uint256"
)

var errStackUnderflow = errors.New("evm: stack underflow")
var errStackOverflow = errors.New("evm: stack overflow")

const maxStack = 1024

type stack struct {
	data []uint256.Int
}

func newStack() *stack { return &stack{data: make([]uint256.Int, 0, 32)} }

func (s *stack) push(v uint256.Int) error {
	if len(s.data) >= maxStack {
		return errStackOverflow
	}
	s.data = append(s.data, v)
	return nil
}

func (s *stack) pop() (uint256.Int, error) {
	if len(s.data) == 0 {
		return uint256.Int{}, errStackUnderflow
	}
	v := s.data[len(s.data)-1]
	s.data = s.data[:len(s.data)-1]
	return v, nil
}

// peek returns the nth element from the top (0 = top) without popping.
func (s *stack) peek(n int) (uint256.Int, error) {
	if n >= len(s.data) {
		return uint256.Int{}, errStackUnderflow
	}
	return s.data[len(s.data)-1-n], nil
}

func (s *stack) dup(n int) error {
	if n > len(s.data) {
		return errStackUnderflow
	}
	v := s.data[len(s.data)-n]
	return s.push(v)
}

func (s *stack) swap(n int) error {
	if n >= len(s.data) {
		return errStackUnderflow
	}
	top := len(s.data) - 1
	s.data[top], s.data[top-n] = s.data[top-n], s.data[top]
	return nil
}

func (s *stack) len() int { return len(s.data) }
