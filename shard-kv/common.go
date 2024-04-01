package shardkv

import (
	"6.824/shardctrler"
	"fmt"
	"time"
)

// OperationOp 定义常规操作类型
type OperationOp uint8
const (
	OpPut OperationOp = iota
	OpAppend
	OpGet
)

// Err 定义错误类型
type Err uint8
const (
	OK Err = iota
	ErrNoKey
	ErrWrongGroup
	ErrWrongLeader
	ErrOutDated
	ErrTimeout
	ErrNotReady
)

type ShardStatus int8
const (
	Serving ShardStatus = iota
	Pulling
	BePulling
	GCing
)
// 用于请求幂等性的操作
type OperationContext struct {
	MaxAppliedCommandId int64
	LastResponse        *CommandResponse
}

func (context OperationContext) deepCopy() OperationContext {
	return OperationContext{context.MaxAppliedCommandId, &CommandResponse{context.LastResponse.Err, context.LastResponse.Value}}
}

func (err Err) String() string {
	switch err {
	case OK:
		return "OK"
	case ErrNoKey:
		return "ErrNoKey"
	case ErrWrongGroup:
		return "ErrWrongGroup"
	case ErrWrongLeader:
		return "ErrWrongLeader"
	case ErrOutDated:
		return "ErrOutDated"
	case ErrTimeout:
		return "ErrTimeout"
	case ErrNotReady:
		return "ErrNotReady"
	}
	panic(fmt.Sprintf("unexpected Err %d", err))
}

type CommandRequest struct {
	Key       string
	Value     string
	Op        OperationOp
	ClientId  int64
	CommandId int64
}

type CommandResponse struct {
	Err   Err
	Value string
}


type Command struct {
	Op   CommandType
	Data interface{}
}

func NewOperationCommand(request *CommandRequest) Command {
	return Command{Operation, *request}
}

func NewConfigurationCommand(config *shardctrler.Config) Command {
	return Command{Configuration, *config}
}

func NewInsertShardsCommand(response *ShardOperationResponse) Command {
	return Command{InsertShards, *response}
}

func NewDeleteShardsCommand(request *ShardOperationRequest) Command {
	return Command{DeleteShards, *request}
}

func NewEmptyEntryCommand() Command {
	return Command{EmptyEntry, nil}
}

type CommandType uint8

const (
	Operation CommandType = iota
	Configuration
	InsertShards
	DeleteShards
	EmptyEntry
)
// 定义shardkv全局超时时间
const (
	ExecuteTimeout            = 500 * time.Millisecond
	ConfigureMonitorTimeout   = 100 * time.Millisecond
	MigrationMonitorTimeout   = 50 * time.Millisecond
	GCMonitorTimeout          = 50 * time.Millisecond
	EmptyEntryDetectorTimeout = 200 * time.Millisecond
)

type ShardOperationRequest struct {
	ConfigNum int
	ShardIDs  []int
}

type ShardOperationResponse struct {
	Err            Err
	ConfigNum      int
	Shards         map[int]map[string]string
	LastOperations map[int64]OperationContext
}