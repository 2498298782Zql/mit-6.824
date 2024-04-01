package shardctrler

import "fmt"

type OperationOp uint8

type CommandRequest struct {
	Servers   map[int][]string // for Join
	GIDs      []int            // for Leave
	Shard     int              // for Move
	GID       int              // for Move
	Num       int              // for Query
	Op        OperationOp
	ClientId  int64
	CommandId int64
}

type CommandResponse struct {
	Err    Err
	Config Config
}

type Command struct {
	*CommandRequest
}

func (request CommandRequest) String() string {
	switch request.Op {
	case OpJoin:
		return fmt.Sprintf("{Servers:%v,Op:%v,ClientId:%v,CommandId:%v}", request.Servers, request.Op, request.ClientId, request.CommandId)
	case OpLeave:
		return fmt.Sprintf("{GIDs:%v,Op:%v,ClientId:%v,CommandId:%v}", request.GIDs, request.Op, request.ClientId, request.CommandId)
	case OpMove:
		return fmt.Sprintf("{Shard:%v,Num:%v,Op:%v,ClientId:%v,CommandId:%v}", request.Shard, request.Num, request.Op, request.ClientId, request.CommandId)
	case OpQuery:
		return fmt.Sprintf("{Num:%v,Op:%v,ClientId:%v,CommandId:%v}", request.Num, request.Op, request.ClientId, request.CommandId)
	}
	panic(fmt.Sprintf("unexpected CommandOp %d", request.Op))
}

func (response CommandResponse) String() string {
	return fmt.Sprintf("{Err:%v,Config:%v}", response.Err, response.Config)
}