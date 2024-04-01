package shardkv

import (
	"6.824/labrpc"
	"6.824/shardctrler"
	"time"
)

//一个clerk代表一个客户端
type Clerk struct {
	sm        *shardctrler.Clerk
	config    shardctrler.Config
	makeEnd   func(string) *labrpc.ClientEnd
	leaderIds map[int]int
	clientId  int64
	commandId int64
}

func MakeClerk(ctrlers []*labrpc.ClientEnd, makeEnd func(string) *labrpc.ClientEnd) *Clerk {
	ck := &Clerk{
		sm:        shardctrler.MakeClerk(ctrlers),
		makeEnd:   makeEnd,
		leaderIds: make(map[int]int),
		clientId:  nrand(),
		commandId: 0,
	}
	ck.config = ck.sm.Query(-1)
	return ck
}

/**
	三种常规操作 取值 修改 追加
 */
func (ck *Clerk) Get(key string) string {
	return ck.Command(&CommandRequest{Key: key, Op: OpGet})
}
func (ck *Clerk) Put(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpPut})
}
func (ck *Clerk) Append(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpAppend})
}



func (ck *Clerk) Command(request *CommandRequest) string {
	request.ClientId, request.CommandId = ck.clientId, ck.commandId
	for {
		shard := key2shard(request.Key)
		gid := ck.config.Shards[shard]
		if servers, ok := ck.config.Groups[gid]; ok {
			if _, ok = ck.leaderIds[gid]; !ok {
				ck.leaderIds[gid] = 0
			}
			oldLeaderId := ck.leaderIds[gid]
			newLeaderId := oldLeaderId
			for {
				var response CommandResponse
				ok := ck.makeEnd(servers[newLeaderId]).Call("ShardKV.Command", request, &response)
				if ok && (response.Err == OK || response.Err == ErrNoKey) {
					ck.commandId++
					return response.Value
				} else if ok && response.Err == ErrWrongGroup {
					break
				} else {
					newLeaderId = (newLeaderId + 1) % len(servers)
					if newLeaderId == oldLeaderId  {
						break
					}
					continue
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
		// ask controller for the latest configuration.
		ck.config = ck.sm.Query(-1)
	}
}

