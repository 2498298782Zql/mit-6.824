package shardctrler

import "fmt"

const NShards = 10

type Config struct {
	Num    int              // config number
	Shards [NShards]int     // shard -> gid
	Groups map[int][]string // gid -> servers[]
}
type Err uint8

const (
	OK Err = iota
	ErrWrongLeader
	ErrTimeout
)

type OperationContext struct {
	MaxAppliedCommandId int64
	LastResponse        *CommandResponse
}

func (err Err) String() string {
	switch err {
	case OK:
		return "OK"
	case ErrWrongLeader:
		return "ErrWrongLeader"
	case ErrTimeout:
		return "ErrTimeout"
	}
	panic(fmt.Sprintf("unexpected Err %d", err))
}

func DefaultConfig() Config {
	return Config{Groups: make(map[int][]string)}
}

const (
	OpJoin OperationOp = iota
	OpLeave
	OpMove
	OpQuery
)

func (op OperationOp) String() string {
	switch op {
	case OpJoin:
		return "OpJoin"
	case OpLeave:
		return "OpLeave"
	case OpMove:
		return "OpMove"
	case OpQuery:
		return "OpQuery"
	}
	panic(fmt.Sprintf("unexpected CommandOp %d", op))
}

